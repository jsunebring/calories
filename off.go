package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Open Food Facts asks every client to identify itself in the User-Agent with a
// way to reach whoever runs it. CONTACT_EMAIL is that address.
var offUserAgent = "calorie-log/1.0 (personal self-hosted tracker; " + env("CONTACT_EMAIL", "change-me@example.com") + ")"

const offFields = "code,product_name,product_name_sv,product_name_en,brands,serving_quantity,nutriments,image_front_small_url"

var httpClient = &http.Client{Timeout: 15 * time.Second}

// productClient is what asks the per-product databases. main gives it a
// transport that keeps each answer, so a copy survives the source changing or
// going away. The Livsmedelsverket sync stays on httpClient: its rows already
// are the copy.
var productClient = &http.Client{Timeout: 15 * time.Second}

// OFFResult separates the two kinds of "no usable answer": the product is not
// in the database at all, or it is there without energy data. Both send the
// user to the manual form, but only the second can prefill a name.
type OFFResult struct {
	Found    bool
	Complete bool
	Food     Food
}

func offLookup(barcode string) (*OFFResult, error) {
	req, _ := http.NewRequest("GET",
		"https://world.openfoodfacts.org/api/v2/product/"+barcode+".json?fields="+offFields, nil)
	req.Header.Set("User-Agent", offUserAgent)
	resp, err := productClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	// A missing product is a 404 WITH a JSON body saying status 0; any other
	// non-200 is the service failing, which must not read as "not found".
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return nil, fmt.Errorf("open food facts: HTTP %d", resp.StatusCode)
	}
	return parseOFF(barcode, body)
}

func parseOFF(barcode string, body []byte) (*OFFResult, error) {
	var d struct {
		Status  int `json:"status"`
		Product struct {
			Name     string         `json:"product_name"`
			NameSv   string         `json:"product_name_sv"`
			NameEn   string         `json:"product_name_en"`
			Brands   string         `json:"brands"`
			ServingQ any            `json:"serving_quantity"`
			Nut      map[string]any `json:"nutriments"`
			Image    string         `json:"image_front_small_url"`
		} `json:"product"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, fmt.Errorf("open food facts: %w", err)
	}
	if d.Status != 1 {
		return &OFFResult{}, nil
	}
	p := d.Product
	name := firstNonEmpty(p.NameSv, p.Name, p.NameEn)
	brand := strings.TrimSpace(strings.Split(p.Brands, ",")[0])
	f := Food{Source: "off", SourceRef: barcode, Barcode: barcode, Name: name, Brand: brand,
		ServingG: num(p.ServingQ), ImageURL: photoLink(p.Image)}
	n := func(k string) *float64 { return num(p.Nut[k+"_100g"]) }

	kcal := n("energy-kcal")
	if kcal == nil {
		if kj := n("energy-kj"); kj != nil {
			v := *kj / 4.184
			kcal = &v
		}
	}
	prot, carbs, fat := n("proteins"), n("carbohydrates"), n("fat")
	complete := kcal != nil && prot != nil && carbs != nil && fat != nil && name != ""
	f.Kcal, f.Protein, f.Carbs, f.Fat = deref(kcal), deref(prot), deref(carbs), deref(fat)
	f.Sugar, f.Fiber, f.Salt, f.SatFat = n("sugars"), n("fiber"), n("salt"), n("saturated-fat")
	return &OFFResult{Found: true, Complete: complete, Food: f}, nil
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x = strings.TrimSpace(x); x != "" {
			return x
		}
	}
	return ""
}

func deref(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// num accepts both shapes OFF uses for numbers: JSON numbers and numeric strings.
func num(v any) *float64 {
	switch x := v.(type) {
	case float64:
		return &x
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(x), 64); err == nil {
			return &f
		}
	}
	return nil
}
