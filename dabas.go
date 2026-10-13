package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Dabas (Delfi) holds producer-entered product data for the Swedish market.
// Its API needs a company agreement; the website's own search endpoint does
// not, and the terms grant free use of the data provided the source is named
// ("Vid användande av Dabas data skall källan anges"), and Delfi may cut off
// anyone it judges to be misusing the service. So ask it one question per
// user action and never bulk-sync it the way the Livsmedelsverket list is.
const dabasSearch = "https://www.dabas.com/api/v2/search"

type dabasHit struct {
	GTIN      string `json:"GTIN"`
	Benamning string `json:"ArtikelBenanmning"`
	Kategori  string `json:"ArtikelKategori"`
	Varumarke string `json:"Varumarke"`
	// The "Thumb" field is the full-size PNG (9.4 MB measured 2026-10-07);
	// Medium is a JPEG of a few tens of kB.
	Bild        string `json:"ProduktbildMedium"`
	Naringsinfo []struct {
		Name  string `json:"Benamning"`
		Value string `json:"Value"`
	} `json:"Naringsinfo"`
}

func dabasQuery(text string) ([]byte, error) {
	req, _ := http.NewRequest("GET", dabasSearch+"?"+url.Values{"SearchText": {text}}.Encode(), nil)
	req.Header.Set("User-Agent", offUserAgent)
	resp, err := productClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("dabas: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

// dabasLookup answers a barcode. Dabas keys products by 14-digit GTIN, so a
// scanned EAN-13 matches with one leading zero; the search accepts either.
func dabasLookup(barcode string) (*OFFResult, error) {
	body, err := dabasQuery(barcode)
	if err != nil {
		return nil, err
	}
	return dabasMatch(body, barcode)
}

func dabasMatch(body []byte, barcode string) (*OFFResult, error) {
	foods, err := parseDabas(body)
	if err != nil {
		return nil, err
	}
	for _, f := range foods {
		// A free-text search can match a barcode inside other fields; only an
		// exact GTIN is an answer.
		if gtinEqual(f.Barcode, barcode) {
			f.Barcode = barcode
			return &OFFResult{Found: true, Complete: true, Food: f}, nil
		}
	}
	return &OFFResult{}, nil
}

func dabasSearchText(q string) ([]Food, error) {
	body, err := dabasQuery(q)
	if err != nil {
		return nil, err
	}
	return parseDabas(body)
}

func gtinEqual(a, b string) bool {
	return strings.TrimLeft(a, "0") == strings.TrimLeft(b, "0")
}

// parseDabas returns the hits that carry the four values a log needs; a
// product without them is left out rather than logged as zero calories.
func parseDabas(body []byte) ([]Food, error) {
	var d struct {
		Results []dabasHit `json:"SearchResults"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("dabas: %w", err)
	}
	out := []Food{}
	for _, h := range d.Results {
		v := map[string]*float64{}
		for _, n := range h.Naringsinfo {
			v[n.Name] = dabasNum(n.Value)
		}
		kcal, prot, carbs, fat := v["energi_kcal"], v["protein"], v["kolhydrat"], v["fett"]
		if kcal == nil && v["energi_kJ"] != nil {
			k := *v["energi_kJ"] / 4.184
			kcal = &k
		}
		if kcal == nil || prot == nil || carbs == nil || fat == nil {
			continue
		}
		gtin := strings.TrimLeft(h.GTIN, "0")
		if len(gtin) < 8 {
			gtin = h.GTIN
		}
		out = append(out, Food{
			Source: "dabas", SourceRef: gtin, Barcode: gtin,
			Name:  firstNonEmpty(h.Benamning, h.Kategori),
			Brand: strings.TrimSpace(h.Varumarke), ImageURL: photoLink(h.Bild),
			Nutrients: Nutrients{Kcal: *kcal, Protein: *prot, Carbs: *carbs, Fat: *fat,
				Sugar: v["sockerarter"], Salt: v["salt"], Fiber: v["fiber"], SatFat: v["mattatfett"]},
		})
	}
	return out, nil
}

// dabasNum reads " 4.9 g", "< 0,1 g" and "373 kcal". A "<" bound is taken at
// its value: for a log, "under 0.1 g" and 0.1 g are the same answer.
func dabasNum(s string) *float64 {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "<"))
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	f, err := strconv.ParseFloat(strings.Replace(s, ",", ".", 1), 64)
	if err != nil {
		return nil
	}
	return &f
}
