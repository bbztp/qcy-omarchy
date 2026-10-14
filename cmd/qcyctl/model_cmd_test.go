package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bbztp/qcy-omarchy/internal/qcycore/models"
)

var webpBytes = append([]byte("RIFF\x10\x00\x00\x00WEBP"), bytes.Repeat([]byte{1}, 32)...)

func ip(n int) *int { return &n }

// imgEnv wires the package seams to an httptest server and a fake product, and
// clears every per-test file. It returns the server and a hit counter.
func imgEnv(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	oldHosts, oldSchemes, oldLookup, oldStatus := imageHosts, imageSchemes, productLookup, modelDaemonStatus
	t.Cleanup(func() {
		imageHosts, imageSchemes, productLookup, modelDaemonStatus = oldHosts, oldSchemes, oldLookup, oldStatus
	})
	host := strings.TrimPrefix(srv.URL, "http://")
	imageHosts = map[string]bool{strings.Split(host, ":")[0]: true}
	imageSchemes = []string{"http"}
	modelDaemonStatus = func() (map[string]interface{}, error) { return nil, errors.New("no daemon") }

	set := func(name string) models.Images {
		return models.Images{
			Icon: srv.URL + "/" + name + "/icon.webp", Left: srv.URL + "/" + name + "/left.webp",
			Right: srv.URL + "/" + name + "/right.webp", Box: srv.URL + "/" + name + "/box.webp",
		}
	}
	prod := models.Product{
		VendorID: 100, Title: "Fake", Images: set("black"),
		Colors: []models.Color{
			{Name: "Black", Index: ip(0), Images: set("black")},
			{Name: "Titanium Gray", Index: ip(1), Images: set("titanium")},
			{Name: "Purple", Index: ip(2), Images: set("purple")},
		},
	}
	productLookup = func(v uint16) (models.Product, bool) {
		if v == 100 {
			return prod, true
		}
		return models.Product{}, false
	}

	root := os.Getenv("XDG_CONFIG_HOME")
	cache := os.Getenv("XDG_CACHE_HOME")
	for _, p := range []string{filepath.Join(root, "qcy-omarchy"), filepath.Join(cache, "qcy-omarchy"), filepath.Join(cache, "qcyctl")} {
		os.RemoveAll(p)
	}
	return srv, &hits
}

func okWebp(w http.ResponseWriter, r *http.Request) { w.Write(webpBytes) }

func runImage(t *testing.T, args ...string) (imageResult, error) {
	t.Helper()
	old := os.Stdout
	rd, wr, _ := os.Pipe()
	os.Stdout = wr
	err := cmdModelImage(append(args, "--json"))
	wr.Close()
	os.Stdout = old
	out, _ := io.ReadAll(rd)
	var res imageResult
	if err == nil {
		if jerr := json.Unmarshal(out, &res); jerr != nil {
			t.Fatalf("bad json %q: %v", out, jerr)
		}
	}
	return res, err
}

func writeCfg(t *testing.T, name, content string) {
	t.Helper()
	d := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "qcy-omarchy")
	os.MkdirAll(d, 0o700)
	if err := os.WriteFile(filepath.Join(d, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeDeviceCache(t *testing.T, line string) {
	t.Helper()
	p := cacheFilePath()
	os.MkdirAll(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

func leftoverTmp(t *testing.T) []string {
	var out []string
	filepath.WalkDir(os.Getenv("XDG_CACHE_HOME"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.Contains(d.Name(), ".tmp") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestImageDownloadAndCacheHit(t *testing.T) {
	_, hits := imgEnv(t, okWebp)
	res, err := runImage(t, "--vendor", "100", "--color", "titanium gray")
	if err != nil {
		t.Fatal(err)
	}
	if res.VendorID != 100 || res.Color.Name != "Titanium Gray" || res.Color.Source != "flag" || res.Color.Index == nil || *res.Color.Index != 1 {
		t.Fatalf("result %+v", res)
	}
	if !res.Network || len(res.Missing) != 0 || len(res.Files) != 4 {
		t.Fatalf("result %+v", res)
	}
	want := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "qcy-omarchy", "images", "100", "titanium-gray", "left.webp")
	if res.Files["left"] != want {
		t.Fatalf("left %q want %q", res.Files["left"], want)
	}
	st, err := os.Stat(want)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v %v", st, err)
	}
	if dst, _ := os.Stat(filepath.Dir(want)); dst.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dst.Mode().Perm())
	}
	if n := atomic.LoadInt32(hits); n != 4 {
		t.Fatalf("%d downloads, want 4", n)
	}
	// Second run: all from cache.
	if _, err := runImage(t, "--vendor", "100", "--color", "1"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 4 {
		t.Fatalf("cache hit downloaded again: %d", n)
	}
	// --refresh downloads again.
	if _, err := runImage(t, "--vendor", "100", "--color", "1", "--refresh"); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 8 {
		t.Fatalf("refresh: %d downloads", n)
	}
	if l := leftoverTmp(t); len(l) != 0 {
		t.Fatalf("tmp files left: %v", l)
	}
}

func TestImageHostOutsideAllowlistRefused(t *testing.T) {
	_, hits := imgEnv(t, okWebp)
	imageHosts = map[string]bool{"img.qcymall.com": true} // the real allowlist
	res, err := runImage(t, "--vendor", "100")
	if err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Fatalf("server contacted %d times", n)
	}
	if len(res.Files) != 0 || len(res.Missing) != 4 {
		t.Fatalf("result %+v", res)
	}
}

func TestImageTooLargeRefused(t *testing.T) {
	_, _ = imgEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(webpBytes)
		w.Write(bytes.Repeat([]byte{0}, imageMaxBytes+10))
	})
	res, err := runImage(t, "--vendor", "100")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 0 || len(res.Missing) != 4 {
		t.Fatalf("result %+v", res)
	}
	if l := leftoverTmp(t); len(l) != 0 {
		t.Fatalf("tmp files left: %v", l)
	}
}

func TestImageNotAnImageRefused(t *testing.T) {
	_, _ = imgEnv(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>nope</html>")) })
	res, err := runImage(t, "--vendor", "100")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 0 {
		t.Fatalf("result %+v", res)
	}
	root := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "qcy-omarchy", "images", "100", "black")
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Fatalf("files written: %v", ents)
	}
}

func TestImagePNGAccepted(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), 0, 0, 0, 0)
	_, _ = imgEnv(t, func(w http.ResponseWriter, r *http.Request) { w.Write(png) })
	res, err := runImage(t, "--vendor", "100")
	if err != nil || len(res.Files) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestImageRedirectToOtherHostRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("redirect target was contacted")
		w.Write(webpBytes)
	}))
	defer other.Close()
	// "localhost" is a different host name than the allowlisted 127.0.0.1.
	target := strings.Replace(other.URL, "127.0.0.1", "localhost", 1)
	_, _ = imgEnv(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+"/x.webp", http.StatusFound)
	})
	res, err := runImage(t, "--vendor", "100")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Files) != 0 || len(res.Missing) != 4 {
		t.Fatalf("result %+v", res)
	}
}

func TestImageRedirectInsideAllowlistFollowed(t *testing.T) {
	_, _ = imgEnv(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/real") {
			http.Redirect(w, r, r.URL.Path+"/real", http.StatusFound)
			return
		}
		w.Write(webpBytes)
	})
	res, err := runImage(t, "--vendor", "100")
	if err != nil || len(res.Files) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestImageAtomicNoTmpAfterFailure(t *testing.T) {
	// A failed write over an existing file must keep the old one intact.
	dest := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "qcy-omarchy", "atomic", "left.webp")
	if err := writeImageAtomic(dest, webpBytes); err != nil {
		t.Fatal(err)
	}
	// Rename onto a directory fails after the tmp file was created.
	bad := filepath.Join(filepath.Dir(dest), "dir.webp")
	os.MkdirAll(filepath.Join(bad, "keep"), 0o700)
	if err := writeImageAtomic(bad, webpBytes); err == nil {
		t.Fatal("expected failure")
	}
	if l := leftoverTmp(t); len(l) != 0 {
		t.Fatalf("tmp files left: %v", l)
	}
	if b, _ := os.ReadFile(dest); !bytes.Equal(b, webpBytes) {
		t.Fatal("existing file damaged")
	}
}

func TestImageColorPrecedence(t *testing.T) {
	_, _ = imgEnv(t, okWebp)

	// no advertised colour known: product default
	writeDeviceCache(t, "AA:BB:CC:DD:EE:FF 0 QCY 100\n")
	res, err := runImage(t, "--no-network")
	if err != nil || res.Color.Source != "default" || res.Color.Name != "Black" || res.VendorID != 100 {
		t.Fatalf("default: %+v %v", res, err)
	}
	// the colour the earbuds advertised beats the default
	writeDeviceCache(t, "AA:BB:CC:DD:EE:FF 0 QCY 100 1\n")
	res, err = runImage(t, "--no-network")
	if err != nil || res.Color.Source != "device" || res.Color.Name != "Titanium Gray" {
		t.Fatalf("device: %+v %v", res, err)
	}
	// the flag beats the advertised colour
	res, err = runImage(t, "--no-network", "--color", "2")
	if err != nil || res.Color.Source != "flag" || res.Color.Name != "Purple" {
		t.Fatalf("flag: %+v %v", res, err)
	}
	// an advertised colour the product does not list falls through to the default
	writeDeviceCache(t, "AA:BB:CC:DD:EE:FF 0 QCY 100 7\n")
	res, err = runImage(t, "--no-network")
	if err != nil || res.Color.Source != "default" {
		t.Fatalf("unlisted: %+v %v", res, err)
	}
	// the old color file is no longer read
	writeCfg(t, "color", "Purple\n")
	writeDeviceCache(t, "AA:BB:CC:DD:EE:FF 0 QCY 100\n")
	res, err = runImage(t, "--no-network")
	if err != nil || res.Color.Source != "default" || res.Color.Name != "Black" {
		t.Fatalf("color file must be ignored: %+v %v", res, err)
	}
	// --vendor for another product ignores the connected unit's colour
	writeDeviceCache(t, "AA:BB:CC:DD:EE:FF 0 QCY 100 1\n")
	res, err = runImage(t, "--no-network", "--vendor", "100")
	if err != nil || res.Color.Source != "device" {
		t.Fatalf("same vendor flag keeps device colour: %+v %v", res, err)
	}
	// an unknown colour on the flag is a usage error
	if _, err = runImage(t, "--color", "Green"); err == nil {
		t.Fatal("unknown --color must fail")
	}
}

func TestResolveColorTable(t *testing.T) {
	byIdx := models.Product{Colors: []models.Color{
		{Name: "A", Index: ip(0)}, {Name: "B", Index: ip(3)},
	}}
	byPos := models.Product{Colors: []models.Color{{Name: "A"}, {Name: "B"}, {Name: "C"}}}
	none := models.Product{}
	cases := []struct {
		name string
		p    models.Product
		dev  int
		have bool
		want string // colour name, or "" for the default
	}{
		{"index field match", byIdx, 3, true, "B"},
		{"index field is not a position", byIdx, 1, true, ""},
		{"position match", byPos, 2, true, "C"},
		{"position out of range", byPos, 5, true, ""},
		{"not advertised", byIdx, 3, false, ""},
		{"no colours", none, 0, true, ""},
	}
	for _, c := range cases {
		ch, err := resolveColor(c.p, true, "", c.dev, c.have)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if c.want == "" && ch.source != "default" || c.want != "" && (ch.source != "device" || ch.name != c.want) {
			t.Errorf("%s: %+v", c.name, ch)
		}
	}
	// every shipped product, every advertised index, with and without a flag: no panic
	for _, p := range models.All() {
		for idx := -1; idx <= 8; idx++ {
			if _, err := resolveColor(p, true, "", idx, true); err != nil {
				t.Errorf("vendor %d idx %d: %v", p.VendorID, idx, err)
			}
		}
		_, _ = resolveColor(p, true, "nope", 0, true)
	}
}

func TestImageNoVendorIsAClearError(t *testing.T) {
	_, _ = imgEnv(t, okWebp)
	_, err := runImage(t)
	if err == nil || !strings.Contains(err.Error(), "--vendor") {
		t.Fatalf("err %v", err)
	}
}

func TestImageUnknownVendor(t *testing.T) {
	_, hits := imgEnv(t, okWebp)
	res, err := runImage(t, "--vendor", "1")
	if err != nil {
		t.Fatal(err)
	}
	if res.VendorID != 1 || len(res.Files) != 0 || len(res.Missing) != 4 || atomic.LoadInt32(hits) != 0 {
		t.Fatalf("%+v hits=%d", res, atomic.LoadInt32(hits))
	}
	if _, err := runImage(t, "--vendor", "abc"); err == nil {
		t.Fatal("non-numeric vendor must fail")
	}
}

func TestImageOffModeOnlyServesCache(t *testing.T) {
	_, hits := imgEnv(t, okWebp)
	if _, err := runImage(t, "--vendor", "100"); err != nil {
		t.Fatal(err)
	}
	base := atomic.LoadInt32(hits)
	if err := cmdModelImages([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	if imagesEnabled() {
		t.Fatal("still enabled")
	}
	// Cached colour is served, an uncached one is missing, nothing downloads.
	res, err := runImage(t, "--vendor", "100", "--color", "Black", "--refresh")
	if err != nil || res.Network || len(res.Files) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	res, err = runImage(t, "--vendor", "100", "--color", "Purple")
	if err != nil || res.Network || len(res.Files) != 0 || len(res.Missing) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	if n := atomic.LoadInt32(hits); n != base {
		t.Fatalf("downloaded while off: %d > %d", n, base)
	}
	if err := cmdModelImages([]string{"on"}); err != nil || !imagesEnabled() {
		t.Fatalf("on: %v", err)
	}
}

func TestImageNetworkDownStopsEarly(t *testing.T) {
	srv, hits := imgEnv(t, okWebp)
	srv.Close() // connections now refused
	res, err := runImage(t, "--vendor", "100")
	if err != nil || len(res.Files) != 0 || len(res.Missing) != 4 {
		t.Fatalf("%+v %v", res, err)
	}
	_ = hits
}

// No URL of an image may be written anywhere but the cache directory.
func TestImageURLsOnlyWrittenUnderCache(t *testing.T) {
	srv, _ := imgEnv(t, okWebp)
	writeDeviceCache(t, "AA:BB:CC:DD:EE:FF 0 QCY 100\n")
	if _, err := runImage(t); err != nil {
		t.Fatal(err)
	}
	if err := cmdModelImages([]string{"off"}); err != nil {
		t.Fatal(err)
	}
	cacheImages := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "qcy-omarchy", "images")
	needle := []byte(strings.TrimPrefix(srv.URL, "http://"))
	filepath.WalkDir(testRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasPrefix(p, cacheImages) {
			return nil
		}
		b, _ := os.ReadFile(p)
		if bytes.Contains(b, needle) || bytes.Contains(b, []byte("img.qcymall.com")) {
			t.Errorf("image URL found outside the image cache: %s", p)
		}
		return nil
	})
	// And the cached files are the image bytes only, not URLs.
	filepath.WalkDir(cacheImages, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); !bytes.Equal(b, webpBytes) {
				t.Errorf("%s: unexpected content", p)
			}
		}
		return nil
	})
}

func TestColorSlug(t *testing.T) {
	cases := []struct {
		in   string
		idx  *int
		want string
	}{
		{"Titanium Gray", nil, "titanium-gray"},
		{"../../etc", nil, "etc"},
		{"  ", ip(3), "color-3"},
		{"", nil, "default"},
		{"Preto/Branco!", nil, "preto-branco"},
	}
	for _, c := range cases {
		if got := colorSlug(c.in, c.idx); got != c.want {
			t.Errorf("%q: %q want %q", c.in, got, c.want)
		}
	}
}

// The real product list resolves the N70 colours with the real allowlist.
func TestImageRealN70Product(t *testing.T) {
	p, ok := models.Lookup(23872)
	if !ok {
		t.Fatal("N70 missing")
	}
	c, ok := findColor(p, "Purple")
	if !ok || c.Index == nil || *c.Index != 2 {
		t.Fatalf("purple %+v", c)
	}
	u := c.Left
	if !strings.HasPrefix(u, "http://img.qcymall.com/") {
		t.Fatalf("left %q", u)
	}
	// Never reaches the network: the allowlist rejects anything else first.
	if err := fetchImage("http://evil.example/x.webp", filepath.Join(t.TempDir(), "x.webp")); err == nil {
		t.Fatal("foreign host accepted")
	}
	if err := fetchImage("ftp://img.qcymall.com/x.webp", filepath.Join(t.TempDir(), "x.webp")); err == nil {
		t.Fatal("ftp accepted")
	}
}
