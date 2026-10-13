package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// Product photos are fetched once and kept as small JPEGs, so the page never
// pulls a source's full-size image and keeps working if the source goes away.

const thumbPx = 200

// imageURLArg is what a newly saved food records about its photo: the
// sources that have product photos answer with a link or with none (""),
// everything else leaves it unknown (NULL).
func imageURLArg(f *Food) any {
	if f.Source == "off" || f.Source == "dabas" {
		return f.ImageURL
	}
	return nil
}

// photoLink returns "" for anything that is not a real photo: Dabas answers a
// product without one with a relative "/files/images/image-missing.png".
func photoLink(u string) string {
	u = strings.TrimSpace(u)
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") || strings.Contains(u, "image-missing") {
		return ""
	}
	return u
}

type pendingImage struct {
	ID      int64
	Source  string
	Barcode string
	URL     *string
}

func (s *Store) PendingImages(limit int) ([]pendingImage, error) {
	rows, err := s.db.Query(`SELECT id, source, COALESCE(barcode,''), image_url FROM foods
		WHERE image_file IS NULL AND image_fails < 3 AND source IN ('off','dabas')
		  AND (image_url <> '' OR (image_url IS NULL AND source = 'off' AND barcode IS NOT NULL))
		ORDER BY id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []pendingImage
	for rows.Next() {
		var p pendingImage
		if err := rows.Scan(&p.ID, &p.Source, &p.Barcode, &p.URL); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) setImage(id int64, url *string, file string, failed bool) error {
	_, err := s.db.Exec(`UPDATE foods SET image_url = COALESCE(?, image_url), image_file = COALESCE(?, image_file),
		image_fails = image_fails + ? WHERE id = ?`, url, nullStr(file), map[bool]int{true: 1}[failed], id)
	return err
}

func (s *Store) ImageFile(id int64) (string, error) {
	var f *string
	err := s.db.QueryRow(`SELECT image_file FROM foods WHERE id=?`, id).Scan(&f)
	if err != nil || f == nil {
		return "", nil
	}
	return *f, nil
}

type imageWorker struct {
	store *Store
	dir   string
	wake  chan struct{}
	// offImage answers an Open Food Facts product's photo link; a product
	// saved before the link was requested has none recorded.
	offImage func(barcode string) (string, error)
	get      func(url string) ([]byte, error)
}

func newImageWorker(store *Store, dir string) *imageWorker {
	return &imageWorker{store: store, dir: dir, wake: make(chan struct{}, 1), offImage: offImageURL, get: fetchImage}
}

func (w *imageWorker) Wake() {
	if w == nil {
		return
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *imageWorker) Loop(ctx context.Context) {
	for {
		for {
			n, err := w.RunOnce()
			if err != nil {
				log.Printf("images: %v", err)
				break
			}
			if n == 0 {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-time.After(time.Hour):
		}
	}
}

// RunOnce handles up to a batch of pending foods and says how many it tried.
func (w *imageWorker) RunOnce() (int, error) {
	pending, err := w.store.PendingImages(20)
	if err != nil {
		return 0, err
	}
	for _, p := range pending {
		url := ""
		if p.URL != nil {
			if url = photoLink(*p.URL); url == "" {
				if err := w.store.setImage(p.ID, &url, "", false); err != nil {
					return 0, err
				}
				continue
			}
		} else {
			if url, err = w.offImage(p.Barcode); err != nil {
				log.Printf("images: food %d: %v", p.ID, err)
				w.store.setImage(p.ID, nil, "", true)
				continue
			}
			if err := w.store.setImage(p.ID, &url, "", false); err != nil {
				return 0, err
			}
			if url == "" {
				continue
			}
		}
		file, err := w.save(p.ID, url)
		if err != nil {
			log.Printf("images: food %d: %v", p.ID, err)
		}
		if err := w.store.setImage(p.ID, nil, file, err != nil); err != nil {
			return 0, err
		}
		time.Sleep(200 * time.Millisecond) // one source's server, one request at a time
	}
	return len(pending), nil
}

func (w *imageWorker) save(id int64, url string) (string, error) {
	data, err := w.get(url)
	if err != nil {
		return "", err
	}
	thumb, err := thumbnail(data)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%d.jpg", id)
	return name, os.WriteFile(filepath.Join(w.dir, name), thumb, 0o644)
}

// thumbnail scales to fit thumbPx and flattens transparency onto white, since
// product PNGs are cut out and JPEG has no alpha.
func thumbnail(data []byte) ([]byte, error) {
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	b := src.Bounds()
	scale := float64(thumbPx) / float64(max(b.Dx(), b.Dy()))
	if scale > 1 {
		scale = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, max(1, int(float64(b.Dx())*scale)), max(1, int(float64(b.Dy())*scale))))
	draw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	var out bytes.Buffer
	err = jpeg.Encode(&out, dst, &jpeg.Options{Quality: 82})
	return out.Bytes(), err
}

// fetchImage uses the plain client: a photo is not an answer worth keeping
// in lookups, and a full-size one is megabytes.
func fetchImage(url string) ([]byte, error) {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", offUserAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

func offImageURL(barcode string) (string, error) {
	req, _ := http.NewRequest("GET", "https://world.openfoodfacts.org/api/v2/product/"+barcode+".json?fields=image_front_small_url", nil)
	req.Header.Set("User-Agent", offUserAgent)
	resp, err := productClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return "", fmt.Errorf("open food facts: HTTP %d", resp.StatusCode)
	}
	var d struct {
		Product struct {
			Image string `json:"image_front_small_url"`
		} `json:"product"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&d); err != nil {
		return "", err
	}
	return d.Product.Image, nil
}

func (s *Server) foodImage(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	name, err := s.store.ImageFile(id)
	if err != nil {
		serverError(w, err)
		return
	}
	if name == "" {
		fail(w, 404, "no image")
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, filepath.Join(s.imageDir, name))
}
