package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Livsmedelsverket's API has no search, only paging and per-food lookups, so
// the whole database is synced into SQLite and searched locally. Each food
// carries a version timestamp; a re-sync fetches nutrients only where it moved.
const slvBase = "https://dataportal.livsmedelsverket.se/livsmedel/api/v1"

type SyncStatus struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Error   string `json:"error,omitempty"`
	Last    string `json:"last,omitempty"`
}

type slvSyncer struct {
	store *Store
	mu    sync.Mutex
	st    SyncStatus
}

func (s *slvSyncer) Status() SyncStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st
}

func (s *slvSyncer) set(f func(*SyncStatus)) {
	s.mu.Lock()
	f(&s.st)
	s.mu.Unlock()
}

func (s *slvSyncer) Loop(every time.Duration) {
	for {
		if err := s.Sync(); err != nil {
			log.Printf("slv sync: %v", err)
		}
		time.Sleep(every)
	}
}

type slvListItem struct {
	Nummer  int    `json:"nummer"`
	Version string `json:"version"`
	Namn    string `json:"namn"`
}

func (s *slvSyncer) Sync() error {
	s.set(func(st *SyncStatus) { *st = SyncStatus{Running: true} })
	err := s.sync()
	s.set(func(st *SyncStatus) {
		st.Running = false
		st.Last = now()
		if err != nil {
			st.Error = err.Error()
		}
	})
	return err
}

func (s *slvSyncer) sync() error {
	body, err := slvGet(slvBase + "/livsmedel?offset=0&limit=10000&sprak=1")
	if err != nil {
		return err
	}
	items, total, err := parseSLVList(body)
	if err != nil {
		return err
	}
	// A short page means the limit was capped server-side; syncing a silent
	// subset would look exactly like a finished sync.
	if len(items) < total {
		return fmt.Errorf("list returned %d of %d foods; paging needed", len(items), total)
	}
	have, err := s.store.SLVVersions()
	if err != nil {
		return err
	}
	var todo []slvListItem
	for _, it := range items {
		if have[strconv.Itoa(it.Nummer)] != slvVersion(it.Version) {
			todo = append(todo, it)
		}
	}
	s.set(func(st *SyncStatus) { st.Total = len(todo) })
	if len(todo) == 0 {
		return nil
	}
	log.Printf("slv sync: %d of %d foods new or changed", len(todo), len(items))

	jobs := make(chan slvListItem)
	var wg sync.WaitGroup
	var failMu sync.Mutex
	failed := 0
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if err := s.syncOne(it); err != nil {
					failMu.Lock()
					failed++
					failMu.Unlock()
					log.Printf("slv %d %q: %v", it.Nummer, it.Namn, err)
				}
				s.set(func(st *SyncStatus) { st.Done++ })
			}
		}()
	}
	for _, it := range todo {
		jobs <- it
	}
	close(jobs)
	wg.Wait()
	if failed > 0 {
		return fmt.Errorf("%d foods failed; they retry on the next sync", failed)
	}
	return nil
}

func (s *slvSyncer) syncOne(it slvListItem) error {
	body, err := slvGet(fmt.Sprintf("%s/livsmedel/%d/naringsvarden?sprak=1", slvBase, it.Nummer))
	if err != nil {
		return err
	}
	n, err := parseSLVNutrients(body)
	if err != nil {
		return err
	}
	f := &Food{Source: "slv", SourceRef: strconv.Itoa(it.Nummer), Name: strings.TrimSpace(it.Namn), Nutrients: *n}
	return s.store.UpsertSLV(f, slvVersion(it.Version))
}

// slvParseRev is bumped when parseSLVNutrients starts reading more values, so
// every food is fetched once more and gains them; their own versions only
// change when Livsmedelsverket edits the food.
const slvParseRev = "2"

func slvVersion(v string) string { return v + "#" + slvParseRev }

func slvGet(url string) ([]byte, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("livsmedelsverket: HTTP %d for %s", resp.StatusCode, url)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

func parseSLVList(body []byte) ([]slvListItem, int, error) {
	var d struct {
		Meta struct {
			TotalRecords int `json:"totalRecords"`
		} `json:"_meta"`
		Livsmedel []slvListItem `json:"livsmedel"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		return nil, 0, fmt.Errorf("livsmedelsverket list: %w", err)
	}
	return d.Livsmedel, d.Meta.TotalRecords, nil
}

// parseSLVNutrients picks the values out of the per-100 g list. Energy appears
// twice under the same euroFIR code, once in kJ and once in kcal; only the
// unit tells them apart.
func parseSLVNutrients(body []byte) (*Nutrients, error) {
	var rows []struct {
		Code  string  `json:"euroFIRkod"`
		Value float64 `json:"varde"`
		Unit  string  `json:"enhet"`
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, fmt.Errorf("livsmedelsverket nutrients: %w", err)
	}
	got := map[string]float64{}
	for _, r := range rows {
		key := r.Code
		if key == "ENERC" {
			key += "_" + r.Unit
		}
		got[key] = r.Value
	}
	need := func(k string) (float64, error) {
		v, ok := got[k]
		if !ok {
			return 0, fmt.Errorf("livsmedelsverket nutrients: no %s", k)
		}
		return v, nil
	}
	opt := func(k string) *float64 {
		if v, ok := got[k]; ok {
			return &v
		}
		return nil
	}
	var n Nutrients
	var err error
	if n.Kcal, err = need("ENERC_kcal"); err != nil {
		return nil, err
	}
	if n.Protein, err = need("PROT"); err != nil {
		return nil, err
	}
	if n.Carbs, err = need("CHO"); err != nil {
		return nil, err
	}
	if n.Fat, err = need("FAT"); err != nil {
		return nil, err
	}
	n.Sugar, n.Fiber, n.Salt = opt("SUGAR"), opt("FIBT"), opt("NACL")
	n.SatFat, n.FreeSugar = opt("FASAT"), opt("SUGFR")
	return &n, nil
}
