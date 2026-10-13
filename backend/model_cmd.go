package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bbztp/qcy-omarchy/backend/internal/qcycore/models"
)

// `qcyctl model ...`: the product images of the user's earbuds. The images are
// not shipped: they are fetched on demand from the QCY CDN (the URLs come from
// the embedded product list) into the user's cache. Only the allowlisted host
// is ever contacted, and `model images off` turns the network access off.

const modelUsage = `usage: qcyctl model <subcommand>
  model image [--vendor N] [--color <name|index>] [--refresh] [--no-network] [--json]   (local paths of the earbuds' images; downloads what is missing)
  model images <on|off|status> [--json]                                                  (allow or forbid downloading the images)`

const (
	// imageMaxBytes is the largest image accepted (the real ones are ~15 KB).
	imageMaxBytes = 2 << 20
	// imageTimeout bounds one download, redirects included.
	imageTimeout = 10 * time.Second
	imageSlots   = 4
)

var (
	// imageHosts is the allowlist of hosts images may be fetched from. Tests
	// override it to point at an httptest server.
	imageHosts = map[string]bool{"img.qcymall.com": true}
	// imageSchemes is the order the schemes are tried in: https first, then http.
	imageSchemes = []string{"https", "http"}
	// productLookup is the product database (a test replaces it).
	productLookup = models.Lookup
	// modelDaemonStatus asks a running daemon for its status; it never starts
	// one nor talks to the earbuds directly. Tests replace it.
	modelDaemonStatus = func() (map[string]interface{}, error) {
		dc, err := dialDaemon(500 * time.Millisecond)
		if err != nil {
			return nil, err
		}
		defer dc.Close()
		return dc.status()
	}
)

var imageNames = [imageSlots]string{"icon", "left", "right", "box"}

type imageColor struct {
	Name   string `json:"name"`
	Index  *int   `json:"index"`
	Source string `json:"source"`
}

type imageResult struct {
	VendorID int               `json:"vendorId"`
	Color    imageColor        `json:"color"`
	Files    map[string]string `json:"files"`
	Missing  []string          `json:"missing"`
	Network  bool              `json:"network"`
}

func cmdModel(args []string) error {
	if len(args) < 1 {
		return errors.New(modelUsage)
	}
	switch args[0] {
	case "image":
		return cmdModelImage(args[1:])
	case "images":
		return cmdModelImages(args[1:])
	}
	return fmt.Errorf("model: unknown subcommand %q\n%s", args[0], modelUsage)
}

// ---- on/off switch ----

func imagesSwitchPath() (string, error) {
	d, err := qcyConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "images"), nil
}

// imagesEnabled is true unless the user turned the downloads off.
func imagesEnabled() bool {
	p, err := imagesSwitchPath()
	if err != nil {
		return true
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return true
	}
	return strings.ToLower(strings.TrimSpace(string(b))) != "off"
}

func cmdModelImages(args []string) error {
	jsonOut := false
	var rest []string
	for _, a := range args {
		if a == "--json" {
			jsonOut = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 {
		return errors.New("usage: qcyctl model images <on|off|status> [--json]")
	}
	p, err := imagesSwitchPath()
	if err != nil {
		return err
	}
	switch rest[0] {
	case "on":
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	case "off":
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte("off\n"), 0o600); err != nil {
			return err
		}
	case "status":
	default:
		return fmt.Errorf("model images: unknown value %q (on|off|status)", rest[0])
	}
	on := imagesEnabled()
	if jsonOut {
		return json.NewEncoder(os.Stdout).Encode(map[string]bool{"enabled": on})
	}
	if on {
		fmt.Println("images: on")
	} else {
		fmt.Println("images: off")
	}
	return nil
}

// ---- cache paths ----

func imageCacheRoot() (string, error) {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "qcy-omarchy", "images"), nil
}

// colorSlug makes a colour name safe as a directory name.
func colorSlug(name string, index *int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		if index != nil {
			return "color-" + strconv.Itoa(*index)
		}
		return "default"
	}
	return s
}

// ---- resolution ----

type colorChoice struct {
	name   string
	index  *int
	urls   [imageSlots]string
	source string
}

func colorURLs(i models.Images) [imageSlots]string {
	return [imageSlots]string{i.Icon, i.Left, i.Right, i.Box}
}

func intPtr(n int) *int { return &n }

// findColor matches a user-written colour (name or index) in the product.
func findColor(p models.Product, spec string) (models.Color, bool) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return models.Color{}, false
	}
	if n, err := strconv.Atoi(spec); err == nil {
		for _, c := range p.Colors {
			if c.Index != nil && *c.Index == n {
				return c, true
			}
		}
		return models.Color{}, false
	}
	for _, c := range p.Colors {
		if strings.EqualFold(strings.TrimSpace(c.Name), spec) {
			return c, true
		}
	}
	return models.Color{}, false
}

func fromColor(c models.Color, source string) colorChoice {
	return colorChoice{name: c.Name, index: c.Index, urls: colorURLs(c.Images), source: source}
}

// defaultColor is the product's default image set, named after the colour it
// equals when there is one, else the first colour, else "default".
func defaultColor(p models.Product) colorChoice {
	if colorURLs(p.Images) != [imageSlots]string{} {
		ch := colorChoice{name: "default", urls: colorURLs(p.Images), source: "default"}
		for _, c := range p.Colors {
			if c.Left != "" && c.Left == p.Images.Left {
				ch.name, ch.index = c.Name, c.Index
				break
			}
		}
		return ch
	}
	for _, c := range p.Colors {
		if !c.Hide {
			return fromColor(c, "default")
		}
	}
	if len(p.Colors) > 0 {
		return fromColor(p.Colors[0], "default")
	}
	return colorChoice{name: "default", source: "default"}
}

// deviceInfo is what the earbuds' cache entry or a running daemon knows.
type deviceInfo struct {
	vendor uint16
	haveV  bool
}

// readDeviceInfo reads the vendorId from the daemon's le-address cache (field
// 4; any later field is ignored) and falls back to a running daemon's status
// when the cache has no vendorId. It never starts a daemon or scans.
func readDeviceInfo() deviceInfo {
	var d deviceInfo
	if p := cacheFilePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			f := strings.Fields(string(b))
			if len(f) > 3 {
				if n, err := strconv.ParseUint(f[3], 10, 16); err == nil {
					d.vendor, d.haveV = uint16(n), true
				}
			}
		}
	}
	if d.haveV {
		return d
	}
	if st, err := modelDaemonStatus(); err == nil {
		if m, ok := st["model"].(map[string]interface{}); ok {
			if v, ok := m["vendorId"].(float64); ok && v >= 0 && v <= 65535 {
				d.vendor, d.haveV = uint16(v), true
			}
		}
	}
	return d
}

func readColorFile() string {
	d, err := qcyConfigDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(d, "color"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveColor applies the precedence flag > file > product default. A bad flag
// is a usage error; a bad file falls through.
//
// The colour the earbuds advertise is deliberately not used: the QCY app
// computes it as (b[3] & 14) >> 1 (bits 1-3 of the manufacturer data), while
// the Quicky scanner delivers (b[3] & 0x18) >> 1 (bits 3-4), which cannot be
// turned back into the app's value. If the scanner ever exposes the raw
// manufacturer bytes, parse (b[3] & 14) >> 1 here and add a "device" source.
func resolveColor(p models.Product, known bool, flag string) (colorChoice, error) {
	if flag != "" {
		if c, ok := findColor(p, flag); ok {
			return fromColor(c, "flag"), nil
		}
		var have []string
		for _, c := range p.Colors {
			s := c.Name
			if c.Index != nil {
				s += "=" + strconv.Itoa(*c.Index)
			}
			have = append(have, s)
		}
		if !known {
			return colorChoice{}, fmt.Errorf("--color %q: unknown vendorId, no colour list to match", flag)
		}
		return colorChoice{}, fmt.Errorf("--color %q: no such colour (have: %s)", flag, strings.Join(have, ", "))
	}
	if f := readColorFile(); f != "" {
		if c, ok := findColor(p, f); ok {
			return fromColor(c, "file"), nil
		}
	}
	return defaultColor(p), nil
}

// ---- command ----

func cmdModelImage(args []string) error {
	var (
		vendorFlag, colorFlag string
		haveVendorFlag        bool
		refresh, noNet        bool
		jsonOut               bool
	)
	for i := 0; i < len(args); i++ {
		a := args[i]
		val := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s needs a value", a)
			}
			i++
			return args[i], nil
		}
		var err error
		switch {
		case a == "--vendor":
			vendorFlag, err = val()
			haveVendorFlag = true
		case strings.HasPrefix(a, "--vendor="):
			vendorFlag, haveVendorFlag = strings.TrimPrefix(a, "--vendor="), true
		case a == "--color":
			colorFlag, err = val()
		case strings.HasPrefix(a, "--color="):
			colorFlag = strings.TrimPrefix(a, "--color=")
		case a == "--refresh":
			refresh = true
		case a == "--no-network":
			noNet = true
		case a == "--json":
			jsonOut = true
		default:
			err = fmt.Errorf("model image: unexpected argument %q\n%s", a, modelUsage)
		}
		if err != nil {
			return err
		}
	}

	var vendor uint16
	if haveVendorFlag {
		n, err := strconv.ParseUint(strings.TrimSpace(vendorFlag), 10, 16)
		if err != nil {
			return fmt.Errorf("--vendor %q: not a vendorId (0-65535)", vendorFlag)
		}
		vendor = uint16(n)
	} else {
		dev := readDeviceInfo()
		if !dev.haveV {
			return errors.New("model image: vendorId unknown (no cached device, no running daemon); pass --vendor N")
		}
		vendor = dev.vendor
	}

	p, known := productLookup(vendor)
	choice, err := resolveColor(p, known, colorFlag)
	if err != nil {
		return err
	}

	root, err := imageCacheRoot()
	if err != nil {
		return err
	}
	dir := filepath.Join(root, strconv.Itoa(int(vendor)), colorSlug(choice.name, choice.index))
	network := imagesEnabled() && !noNet

	res := imageResult{
		VendorID: int(vendor),
		Color:    imageColor{Name: choice.name, Index: choice.index, Source: choice.source},
		Files:    map[string]string{},
		Missing:  []string{},
		Network:  network,
	}
	netBroken := false
	for i, slot := range imageNames {
		path := filepath.Join(dir, slot+".webp")
		have := fileExists(path)
		if (!have || refresh) && network && !netBroken && choice.urls[i] != "" {
			if err := fetchImage(choice.urls[i], path); err != nil {
				fmt.Fprintf(os.Stderr, "qcyctl: model image: %s: %v\n", slot, err)
				var ne *netError
				if errors.As(err, &ne) {
					netBroken = true // the network is down: do not wait 10 s per file
				}
			} else {
				have = true
			}
		}
		if have {
			res.Files[slot] = path
		} else {
			res.Missing = append(res.Missing, slot)
		}
	}

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}
	idx := "-"
	if res.Color.Index != nil {
		idx = strconv.Itoa(*res.Color.Index)
	}
	fmt.Printf("vendorId %d, color %s (index %s, from %s), network %v\n", res.VendorID, res.Color.Name, idx, res.Color.Source, res.Network)
	for _, slot := range imageNames {
		if f, ok := res.Files[slot]; ok {
			fmt.Printf("  %-5s %s\n", slot, f)
		}
	}
	if len(res.Missing) > 0 {
		fmt.Printf("  missing: %s\n", strings.Join(res.Missing, ", "))
	}
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular() && st.Size() > 0
}

// ---- download ----

// netError marks a transport failure (as opposed to a refused or bad reply).
type netError struct{ err error }

func (e *netError) Error() string { return e.err.Error() }
func (e *netError) Unwrap() error { return e.err }

func imageHostAllowed(u *url.URL) bool {
	if u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return imageHosts[strings.ToLower(u.Hostname())]
}

func newImageClient() *http.Client {
	return &http.Client{
		Timeout: imageTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if !imageHostAllowed(req.URL) {
				return fmt.Errorf("redirect to %s refused (host not allowed)", req.URL.Hostname())
			}
			return nil
		},
	}
}

// looksLikeImage checks the magic bytes of WebP (RIFF....WEBP) and PNG.
func looksLikeImage(b []byte) bool {
	if len(b) >= 12 && string(b[0:4]) == "RIFF" && string(b[8:12]) == "WEBP" {
		return true
	}
	return len(b) >= 8 && string(b[0:8]) == "\x89PNG\r\n\x1a\n"
}

// fetchImage downloads rawURL to dest (atomically). The scheme is tried as
// https first, then http; the host must be on the allowlist.
func fetchImage(rawURL, dest string) error {
	u, err := url.Parse(rawURL)
	if err != nil || !imageHostAllowed(u) {
		return fmt.Errorf("refusing %q: host not on the allowlist", rawURL)
	}
	client := newImageClient()
	var lastErr error
	for _, scheme := range imageSchemes {
		cu := *u
		cu.Scheme = scheme
		data, err := getImage(client, cu.String())
		if err == nil {
			return writeImageAtomic(dest, data)
		}
		lastErr = err
		var ne *netError
		if !errors.As(err, &ne) {
			break // a reply that is wrong stays wrong on the other scheme
		}
	}
	return lastErr
}

func getImage(client *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "qcyctl")
	resp, err := client.Do(req)
	if err != nil {
		// A refused redirect is a policy result, not a dead network.
		if strings.Contains(err.Error(), "refused (host not allowed)") {
			return nil, err
		}
		return nil, &netError{err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > imageMaxBytes {
		return nil, fmt.Errorf("image too large (%d bytes, max %d)", resp.ContentLength, imageMaxBytes)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, imageMaxBytes+1))
	if err != nil {
		return nil, &netError{err}
	}
	if len(data) > imageMaxBytes {
		return nil, fmt.Errorf("image too large (max %d bytes)", imageMaxBytes)
	}
	if !looksLikeImage(data) {
		return nil, errors.New("reply is not a WebP/PNG image")
	}
	return data, nil
}

func writeImageAtomic(dest string, data []byte) (err error) {
	dir := filepath.Dir(dest)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(dest)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
