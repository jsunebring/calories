package main

import (
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

//go:embed web
var webFS embed.FS

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	dataDir := env("DATA_DIR", "./data")
	photoDir, imageDir := filepath.Join(dataDir, "photos"), filepath.Join(dataDir, "food-images")
	for _, d := range []string{photoDir, imageDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			log.Fatal(err)
		}
	}
	store, err := OpenStore(filepath.Join(dataDir, "calorie-log.db"))
	if err != nil {
		log.Fatal(err)
	}
	if len(os.Args) > 1 && os.Args[1] == "user" {
		if err := userCommand(store, os.Args[2:], os.Stdout); err != nil {
			log.Fatal(err)
		}
		return
	}
	productClient.Transport = recordingTransport{store, http.DefaultTransport}
	syncer := &slvSyncer{store: store}
	if os.Getenv("SLV_SYNC") != "off" {
		go syncer.Loop(7 * 24 * time.Hour)
	}

	// Dabas first: its data is entered by the producers themselves, where Open
	// Food Facts is crowd-edited.
	srv := &Server{store: store, slv: syncer, photoDir: photoDir, imageDir: imageDir, images: newImageWorker(store, imageDir), dabasSearch: dabasSearchText,
		sources: []barcodeSource{{"Dabas", dabasLookup}, {"Open Food Facts", offLookup}}}
	srv.sharedKey, srv.model = os.Getenv("ANTHROPIC_API_KEY"), env("ESTIMATE_MODEL", defaultEstimateModel)
	if team, aud := os.Getenv("CF_ACCESS_TEAM"), os.Getenv("CF_ACCESS_AUD"); team != "" && aud != "" {
		srv.access = newAccessVerifier(team, aud)
		log.Printf("Cloudflare Access identity on, issuer %s", srv.access.issuer)
	}
	if secret := os.Getenv("KEY_SECRET"); secret != "" {
		if srv.keys, err = newKeyBox(secret); err != nil {
			log.Fatal(err)
		}
	} else {
		log.Printf("KEY_SECRET not set: users cannot save their own API keys")
	}
	if _, ok := modelPrices[srv.model]; !ok {
		log.Printf("no price known for %s: estimates will show no cost", srv.model)
	}
	go srv.images.Loop(context.Background())
	mux := http.NewServeMux()
	srv.routes(mux)
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", noCache(http.FileServerFS(web)))

	addr := ":" + env("PORT", "8080")
	log.Printf("calorie-log listening on %s, data in %s", addr, dataDir)
	log.Fatal(http.ListenAndServe(addr, srv.onlyVerifiedFromCloudflare(mux)))
}

// The page is one file that changes with every deploy; a phone holding a
// cached copy against a newer API is the failure this avoids.
func noCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}
