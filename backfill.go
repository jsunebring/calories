package main

import (
	"log"
	"strings"
)

// backfillFromLookups fills values a parser has learned to read since a
// scanned product was saved, from the raw answers kept in lookups, so nothing
// has to be asked of the source again. Only empty columns are written.
func (s *Store) backfillFromLookups() error {
	rows, err := s.db.Query(`SELECT f.id, f.source, f.barcode, l.url, l.body FROM foods f
		JOIN lookups l ON l.url LIKE '%' || f.barcode || '%'
		WHERE f.source IN ('off','dabas') AND f.sat_fat IS NULL AND f.barcode IS NOT NULL`)
	if err != nil {
		return err
	}
	type fill struct {
		id     int64
		satFat float64
	}
	var fills []fill
	for rows.Next() {
		var id int64
		var source, barcode, url string
		var body []byte
		if err := rows.Scan(&id, &source, &barcode, &url, &body); err != nil {
			rows.Close()
			return err
		}
		var r *OFFResult
		switch {
		case source == "off" && strings.Contains(url, "openfoodfacts"):
			r, err = parseOFF(barcode, body)
		case source == "dabas" && strings.Contains(url, "dabas"):
			r, err = dabasMatch(body, barcode)
		default:
			continue
		}
		if err == nil && r.Found && r.Food.SatFat != nil {
			fills = append(fills, fill{id, *r.Food.SatFat})
		}
	}
	rows.Close()
	for _, f := range fills {
		if _, err := s.db.Exec(`UPDATE foods SET sat_fat=? WHERE id=? AND sat_fat IS NULL`, f.satFat, f.id); err != nil {
			return err
		}
	}
	n, err := s.backfillDabasSearches()
	if err != nil {
		return err
	}
	if total := len(fills) + n; total > 0 {
		log.Printf("backfill: saturated fat for %d foods from kept lookups", total)
	}
	return nil
}

// backfillDabasSearches covers foods kept from a text search, whose answer is
// stored under the search words rather than the barcode: every kept Dabas
// answer is parsed and matched by GTIN.
func (s *Store) backfillDabasSearches() (int, error) {
	rows, err := s.db.Query(`SELECT body FROM lookups WHERE host LIKE '%dabas%'`)
	if err != nil {
		return 0, err
	}
	sat := map[string]float64{}
	img := map[string]string{}
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return 0, err
		}
		foods, err := parseDabas(body)
		if err != nil {
			continue
		}
		for _, f := range foods {
			code := strings.TrimLeft(f.Barcode, "0")
			if f.SatFat != nil {
				sat[code] = *f.SatFat
			}
			img[code] = f.ImageURL
		}
	}
	rows.Close()
	if len(img) == 0 {
		return 0, nil
	}
	if err := s.backfillDabasImages(img); err != nil {
		return 0, err
	}
	foods, err := s.db.Query(`SELECT id, barcode FROM foods WHERE source='dabas' AND sat_fat IS NULL AND barcode IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type fill struct {
		id int64
		v  float64
	}
	var fills []fill
	for foods.Next() {
		var id int64
		var code string
		if err := foods.Scan(&id, &code); err != nil {
			foods.Close()
			return 0, err
		}
		if v, ok := sat[strings.TrimLeft(code, "0")]; ok {
			fills = append(fills, fill{id, v})
		}
	}
	foods.Close()
	for _, f := range fills {
		if _, err := s.db.Exec(`UPDATE foods SET sat_fat=? WHERE id=?`, f.v, f.id); err != nil {
			return 0, err
		}
	}
	return len(fills), nil
}

// backfillDabasImages records the photo link for Dabas foods saved before
// links were read. It only writes where nothing is recorded yet.
func (s *Store) backfillDabasImages(img map[string]string) error {
	rows, err := s.db.Query(`SELECT id, barcode FROM foods WHERE source='dabas' AND image_url IS NULL AND barcode IS NOT NULL`)
	if err != nil {
		return err
	}
	type fill struct {
		id  int64
		url string
	}
	var fills []fill
	for rows.Next() {
		var id int64
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			rows.Close()
			return err
		}
		if u, ok := img[strings.TrimLeft(code, "0")]; ok {
			fills = append(fills, fill{id, u})
		}
	}
	rows.Close()
	for _, f := range fills {
		if _, err := s.db.Exec(`UPDATE foods SET image_url=? WHERE id=? AND image_url IS NULL`, f.url, f.id); err != nil {
			return err
		}
	}
	return nil
}
