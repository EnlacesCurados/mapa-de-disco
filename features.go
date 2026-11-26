package main

// Cleanup candidates, duplicate search and comparison with the previous scan.

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const mb = int64(1) << 20

// ---------- comparison with the previous scan ----------

type snapshot struct {
	T     int64            `json:"t"`
	Total int64            `json:"total"`
	Thr   int64            `json:"thr"`
	Dirs  map[string]int64 `json:"dirs"`
}

type historyFile struct {
	Root  string     `json:"root"`
	Snaps []snapshot `json:"snaps"`
}

func historyPath(root string) string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	h := sha1.Sum([]byte(strings.ToLower(root)))
	return filepath.Join(dir, "MapaDeDisco", "history", hex.EncodeToString(h[:8])+".json")
}

func setDeltas(n *Node, rel string, base *snapshot) {
	if prev, ok := base.Dirs[rel]; ok {
		n.Delta = n.Size - prev
	} else if n.Size > 2*base.Thr {
		n.Delta = n.Size // was not there before, or was too small to be recorded
	}
	for _, c := range n.Children {
		if c.Dir && c.Size >= base.Thr/2 {
			r := c.Name
			if rel != "" {
				r = rel + string(os.PathSeparator) + c.Name
			}
			setDeltas(c, r, base)
		}
	}
}

func collectDirs(n *Node, rel string, thr int64, out map[string]int64) {
	if len(out) >= 20000 {
		return
	}
	out[rel] = n.Size
	for _, c := range n.Children {
		if c.Dir && c.Size >= thr {
			r := c.Name
			if rel != "" {
				r = rel + string(os.PathSeparator) + c.Name
			}
			collectDirs(c, r, thr, out)
		}
	}
}

// applyHistory compares the fresh tree with an earlier scan of the same folder
// and records a summary of this one. Only folder sizes are stored, never file names.
func applyHistory(s *scanState, t *Node) {
	p := historyPath(s.root)
	if p == "" {
		return
	}
	var hf historyFile
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &hf)
	}
	now := time.Now().Unix()
	var base *snapshot
	for i := len(hf.Snaps) - 1; i >= 0; i-- {
		if now-hf.Snaps[i].T >= 12*3600 {
			base = &hf.Snaps[i]
			break
		}
	}
	if base == nil && len(hf.Snaps) > 0 && now-hf.Snaps[0].T >= 60 {
		base = &hf.Snaps[0]
	}
	if base != nil {
		s.prevTime = base.T
		setDeltas(t, "", base)
	}
	if len(hf.Snaps) > 0 && now-hf.Snaps[len(hf.Snaps)-1].T < 3600 {
		return // at most one summary per hour
	}
	thr := 20 * mb
	if t.Size/5000 > thr {
		thr = t.Size / 5000
	}
	snap := snapshot{T: now, Total: t.Size, Thr: thr, Dirs: map[string]int64{}}
	collectDirs(t, "", thr, snap.Dirs)
	hf.Root = s.root
	hf.Snaps = append(hf.Snaps, snap)
	if len(hf.Snaps) > 12 {
		hf.Snaps = hf.Snaps[len(hf.Snaps)-12:]
	}
	if b, err := json.Marshal(hf); err == nil {
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, b, 0o644)
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

type changeItem struct {
	P string `json:"p"`
	S int64  `json:"s"`
	G int64  `json:"g"`
}

func collectChanges(n *Node, path string, thr int64, out *[]changeItem) {
	explained := false
	for _, c := range n.Children {
		if !c.Dir {
			continue
		}
		if abs64(c.Delta) >= thr || c.Size >= thr {
			collectChanges(c, filepath.Join(path, c.Name), thr, out)
		}
		if n.Delta != 0 && (c.Delta > 0) == (n.Delta > 0) && abs64(c.Delta)*10 >= abs64(n.Delta)*8 {
			explained = true // one subfolder accounts for most of the change: show that one instead
		}
	}
	if abs64(n.Delta) >= thr && !explained {
		*out = append(*out, changeItem{path, n.Size, n.Delta})
	}
}

func handleChanges(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	p := filepath.Clean(r.URL.Query().Get("path"))
	n, _ := findNode(cur, p)
	if n == nil || !n.Dir {
		fail(w, 404, "gone")
		return
	}
	items := []changeItem{}
	if cur.prevTime != 0 {
		thr := 50 * mb
		if abs64(n.Delta)/200 > thr {
			thr = abs64(n.Delta) / 200
		}
		for _, c := range n.Children {
			if c.Dir && (abs64(c.Delta) >= thr || c.Size >= thr) {
				collectChanges(c, filepath.Join(p, c.Name), thr, &items)
			}
		}
		sort.Slice(items, func(i, j int) bool { return items[i].G > items[j].G })
		if len(items) > 40 {
			items = append(items[:30:30], items[len(items)-10:]...)
		}
	}
	writeJSON(w, map[string]interface{}{"prev": cur.prevTime, "delta": n.Delta, "items": items})
}

// ---------- cleanup candidates ----------

var cacheDirs = map[string]bool{}
var coreDirs = map[string]bool{"windows": true, "program files": true, "program files (x86)": true, "programdata": true, "$recycle.bin": true, "system volume information": true}
var downloadDirs = map[string]bool{"downloads": true, "descargas": true}
var tmpExts = map[string]bool{"tmp": true, "temp": true, "dmp": true, "crdownload": true, "part": true}
var installerExts = map[string]bool{"exe": true, "msi": true, "msix": true, "iso": true, "dmg": true, "pkg": true}

func init() {
	for _, n := range strings.Split("node_modules|__pycache__|.cache|cache|caches|code cache|gpucache|cacheclip|media cache files|media cache|peak files|temp|tmp|.tmp|.gradle|.pytest_cache|.mypy_cache|shadercache|shader cache|crashdumps|crash reports", "|") {
		cacheDirs[n] = true
	}
}

func extOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}

type cleanItem struct {
	P string `json:"p"`
	S int64  `json:"s"`
	M int64  `json:"m"`
	D int    `json:"d,omitempty"`
	C uint8  `json:"c"`
	F int64  `json:"f,omitempty"`
}

type cleanGroup struct {
	Total int64       `json:"total"`
	Count int         `json:"count"`
	Items []cleanItem `json:"items"`
}

func (g *cleanGroup) add(it cleanItem) {
	g.Total += it.S
	g.Count++
	g.Items = append(g.Items, it)
}

func (g *cleanGroup) finish() {
	sort.Slice(g.Items, func(i, j int) bool { return g.Items[i].S > g.Items[j].S })
	if len(g.Items) > 300 {
		g.Items = g.Items[:300]
	}
	if g.Items == nil {
		g.Items = []cleanItem{}
	}
}

type cleanWalk struct {
	caches, installers, old cleanGroup
	cutoff                  int64
}

func (cw *cleanWalk) walk(n *Node, path string, inDownloads, inAppData bool) {
	for _, c := range n.Children {
		p := filepath.Join(path, c.Name)
		if c.Dir {
			ln := strings.ToLower(c.Name)
			if coreDirs[ln] {
				continue // never suggest anything inside system folders
			}
			if cacheDirs[ln] {
				if c.Size >= mb {
					cw.caches.add(cleanItem{p, c.Size, c.Mtime, 1, 0, c.Files})
				}
				continue
			}
			cw.walk(c, p, downloadDirs[ln], inAppData || ln == "appdata")
			continue
		}
		ext := extOf(c.Name)
		switch {
		case tmpExts[ext]:
			if c.Size >= mb {
				cw.caches.add(cleanItem{p, c.Size, c.Mtime, 0, c.Cat, 0})
			}
		case inDownloads && installerExts[ext]:
			cw.installers.add(cleanItem{p, c.Size, c.Mtime, 0, c.Cat, 0})
		case c.Size >= 100*mb && c.Mtime > 0 && c.Mtime < cw.cutoff && c.Cat != 6 && !inAppData:
			cw.old.add(cleanItem{p, c.Size, c.Mtime, 0, c.Cat, 0})
		}
	}
}

func handleCleanup(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	p := filepath.Clean(r.URL.Query().Get("path"))
	n, _ := findNode(cur, p)
	if n == nil || !n.Dir {
		fail(w, 404, "gone")
		return
	}
	cw := &cleanWalk{cutoff: time.Now().Unix() - 365*86400}
	cw.walk(n, p, downloadDirs[strings.ToLower(n.Name)] || downloadDirs[strings.ToLower(filepath.Base(p))], strings.Contains(strings.ToLower(p), string(os.PathSeparator)+"appdata"))
	cw.caches.finish()
	cw.installers.finish()
	cw.old.finish()
	writeJSON(w, map[string]interface{}{"caches": cw.caches, "installers": cw.installers, "old": cw.old})
}

// ---------- duplicates ----------

type dupFile struct {
	P string `json:"p"`
	M int64  `json:"m"`
	C uint8  `json:"c"`
}

type dupGroup struct {
	S     int64     `json:"s"`
	Files []dupFile `json:"files"`
}

var dup struct {
	sync.Mutex
	state       string
	path        string
	gen         int
	done, total atomic.Int64
	groups      []dupGroup
}

type dupCand struct {
	path  string
	size  int64
	mtime int64
	cat   uint8
}

func collectCands(n *Node, path string, out *[]dupCand) {
	for _, c := range n.Children {
		if c.Size < 10*mb {
			break // children are sorted by size
		}
		p := filepath.Join(path, c.Name)
		if c.Dir {
			if !coreDirs[strings.ToLower(c.Name)] {
				collectCands(c, p, out)
			}
		} else {
			*out = append(*out, dupCand{p, c.Size, c.Mtime, c.Cat})
		}
	}
}

// signature identifies a file's content. Files up to 64 MB are hashed in full;
// larger ones by size plus 18 samples spread across the file.
func signature(path string, size int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if size <= 64*mb {
		if _, err := io.Copy(h, f); err != nil {
			return "", err
		}
	} else {
		buf := make([]byte, mb)
		read := func(off int64, n int64) {
			m, _ := f.ReadAt(buf[:n], off)
			h.Write(buf[:m])
		}
		read(0, mb)
		read(size-mb, mb)
		for i := int64(1); i <= 16; i++ {
			read(size/17*i, 64<<10)
		}
	}
	fmt.Fprintf(h, "|%d", size)
	return hex.EncodeToString(h.Sum(nil)), nil
}

func startDups(path string) bool {
	mu.RLock()
	n, _ := findNode(cur, path)
	var cands []dupCand
	if n != nil && n.Dir {
		collectCands(n, path, &cands)
	}
	mu.RUnlock()
	if n == nil || !n.Dir {
		return false
	}
	bySize := map[int64][]dupCand{}
	for _, c := range cands {
		bySize[c.size] = append(bySize[c.size], c)
	}
	var todo []dupCand
	for _, g := range bySize {
		if len(g) > 1 {
			todo = append(todo, g...)
		}
	}
	dup.Lock()
	dup.gen++
	gen := dup.gen
	dup.state, dup.path, dup.groups = "running", path, nil
	dup.done.Store(0)
	dup.total.Store(int64(len(todo)))
	dup.Unlock()

	go func() {
		type res struct {
			c   dupCand
			sig string
		}
		jobs, results := make(chan dupCand), make(chan res)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for c := range jobs {
					sig, err := signature(c.path, c.size)
					dup.done.Add(1)
					if err == nil {
						results <- res{c, sig}
					}
				}
			}()
		}
		go func() {
			for _, c := range todo {
				dup.Lock()
				stale := dup.gen != gen
				dup.Unlock()
				if stale {
					break
				}
				jobs <- c
			}
			close(jobs)
			wg.Wait()
			close(results)
		}()
		bySig := map[string][]dupCand{}
		for r := range results {
			bySig[r.sig] = append(bySig[r.sig], r.c)
		}
		var groups []dupGroup
		for _, g := range bySig {
			if len(g) < 2 {
				continue
			}
			sort.Slice(g, func(i, j int) bool {
				if g[i].mtime != g[j].mtime {
					return g[i].mtime < g[j].mtime
				}
				return g[i].path < g[j].path
			})
			dg := dupGroup{S: g[0].size}
			for _, c := range g {
				dg.Files = append(dg.Files, dupFile{c.path, c.mtime, c.cat})
			}
			groups = append(groups, dg)
		}
		sort.Slice(groups, func(i, j int) bool {
			return groups[i].S*int64(len(groups[i].Files)-1) > groups[j].S*int64(len(groups[j].Files)-1)
		})
		dup.Lock()
		if dup.gen == gen {
			dup.groups, dup.state = groups, "done"
		}
		dup.Unlock()
	}()
	return true
}

func handleDups(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var body struct{ Path string }
		if json.NewDecoder(r.Body).Decode(&body) != nil || !startDups(filepath.Clean(body.Path)) {
			fail(w, 400, "gone")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	dup.Lock()
	state, path, groups := dup.state, dup.path, dup.groups
	dup.Unlock()
	if state == "" {
		state = "idle"
	}
	out := []dupGroup{}
	var waste int64
	mu.RLock()
	if path != "" && cur.tree != nil {
		if n, _ := findNode(cur, path); n == nil {
			state, groups = "idle", nil // belongs to an earlier scan
		}
	}
	for _, g := range groups {
		ng := dupGroup{S: g.S}
		for _, f := range g.Files {
			if n, _ := findNode(cur, f.P); n != nil {
				ng.Files = append(ng.Files, f)
			}
		}
		if len(ng.Files) > 1 {
			waste += ng.S * int64(len(ng.Files)-1)
			if len(out) < 200 {
				out = append(out, ng)
			}
		}
	}
	mu.RUnlock()
	writeJSON(w, map[string]interface{}{"state": state, "path": path, "done": dup.done.Load(), "total": dup.total.Load(), "groups": out, "waste": waste})
}
