package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParsersReadThePhotoLink(t *testing.T) {
	hits, _ := parseDabas(fixture(t, "dabas-search-pagen-gifflar.json"))
	if !strings.HasSuffix(strings.Split(hits[0].ImageURL, "?")[0], "_medium.jpg") {
		t.Fatalf("Dabas: want the medium JPEG, not the multi-MB 'thumb' PNG: %q", hits[0].ImageURL)
	}
	r, _ := parseOFF("7300400481588", fixture(t, "off-wasa-with-image.json"))
	if !strings.Contains(r.Food.ImageURL, "images.openfoodfacts.org") {
		t.Fatalf("Open Food Facts: %q", r.Food.ImageURL)
	}
}

// A cut-out product PNG, larger than a thumbnail, transparent at the corner.
func cutoutPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	for y := 100; y < 500; y++ {
		for x := 100; x < 700; x++ {
			img.Set(x, y, color.NRGBA{200, 40, 40, 255})
		}
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

func TestImagesAreFetchedOnceAsSmallJPEGs(t *testing.T) {
	store := testStore(t)
	dir := t.TempDir()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(cutoutPNG()) }))
	defer src.Close()

	dabas := &Food{Source: "dabas", SourceRef: "1", Barcode: "7311070006209", Name: "Gifflar", ImageURL: src.URL + "/g.png"}
	oldOFF := &Food{Source: "off", SourceRef: "2", Barcode: "7300400481588", Name: "Wasa"}
	noPhoto := &Food{Source: "off", SourceRef: "3", Barcode: "5449000000996", Name: "Cola"}
	missing := &Food{Source: "dabas", SourceRef: "4", Barcode: "7310000000001", Name: "Okänd"}
	for _, f := range []*Food{dabas, oldOFF, noPhoto, missing} {
		store.SaveFood(f, "")
	}
	// A product saved before links were requested has none recorded at all.
	store.db.Exec(`UPDATE foods SET image_url = NULL WHERE source = 'off'`)
	// Stored before photoLink existed: the placeholder as the link.
	store.db.Exec(`UPDATE foods SET image_url = '/files/images/image-missing.png' WHERE id = ?`, missing.ID)

	asked := map[string]int{}
	w := newImageWorker(store, dir)
	w.offImage = func(code string) (string, error) {
		asked[code]++
		if code == noPhoto.Barcode {
			return "", nil
		}
		return src.URL + "/w.png", nil
	}
	if _, err := w.RunOnce(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []*Food{dabas, oldOFF} {
		got, _ := store.FoodByID(f.ID)
		if !got.HasImage {
			t.Fatalf("%s has no image", f.Name)
		}
		data, _ := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.jpg", f.ID)))
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil || img.Bounds().Dx() > thumbPx || img.Bounds().Dy() > thumbPx {
			t.Fatalf("%s: want a JPEG within %dpx: %v %v", f.Name, thumbPx, err, img.Bounds())
		}
		if r, g, b, _ := img.At(0, 0).RGBA(); r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
			t.Fatalf("transparency must become white, got %v", img.At(0, 0))
		}
	}
	var fails int
	store.db.QueryRow(`SELECT image_fails FROM foods WHERE id=?`, missing.ID).Scan(&fails)
	if fails != 0 {
		t.Fatal("Dabas's image-missing placeholder means no photo, not a failed download")
	}
	if n, _ := w.RunOnce(); n != 0 || asked[noPhoto.Barcode] != 1 {
		t.Fatalf("a product the source has no photo for must not be asked again: pending %d, asked %d", n, asked[noPhoto.Barcode])
	}

	srv := &Server{store: store, imageDir: dir}
	resp := serve(t, srv, httptest.NewRequest("GET", fmt.Sprintf("/api/food-images/%d", dabas.ID), nil))
	if resp.Code != 200 || resp.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("serve: %d %s", resp.Code, resp.Header().Get("Content-Type"))
	}
}
