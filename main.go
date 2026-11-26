// Mapa de Disco: a visual disk space analyzer.
// One executable: it scans folders and serves the interface to your browser (127.0.0.1 only).
package main

import (
	"container/heap"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed web/index.html
var indexHTML string

const nCats = 8

// Categories: 0 video, 1 image, 2 audio, 3 documents, 4 archives, 5 code and data, 6 programs and system, 7 other
var extCat = map[string]uint8{}

func init() {
	groups := map[uint8]string{
		0: "mp4 mov avi mkv wmv flv webm m4v mpg mpeg mts m2ts 3gp vob mxf r3d braw prores ts ogv insv lrv dv",
		1: "jpg jpeg png gif bmp tif tiff webp heic heif raw cr2 cr3 nef arw dng orf rw2 psd psb ai svg ico exr hdr tga eps indd xcf kra avif dpx",
		2: "mp3 wav flac aac ogg m4a wma aiff aif opus mid midi amr ape alac",
		3: "pdf doc docx xls xlsx ppt pptx odt ods odp txt rtf md epub mobi csv pages numbers key one msg eml djvu tex",
		4: "zip rar 7z tar gz bz2 xz tgz iso cab dmg zst img vhd vhdx wim esd",
		5: "js ts jsx tsx py go rs c cpp h hpp java cs php rb html htm css scss json xml yaml yml sql db sqlite sqlite3 mdb accdb ipynb sh bat ps1 lua swift kt safetensors ckpt pt pth gguf onnx bin dat pak npy npz parquet log",
		6: "exe dll sys msi msp msix appx drv ocx cpl scr com lnk apk jar so dylib mui cat inf pdb lib obj tmp etl dmp",
	}
	for c, list := range groups {
		for _, e := range strings.Fields(list) {
			extCat[e] = c
		}
	}
}

func catOf(name string) uint8 {
	i := strings.LastIndexByte(name, '.')
	if i < 0 || i == len(name)-1 {
		return 7
	}
	if c, ok := extCat[strings.ToLower(name[i+1:])]; ok {
		return c
	}
	return 7
}

type Node struct {
	Name     string
	Size     int64
	Mtime    int64
	Children []*Node
	Files    int64
	Delta    int64
	Cats     *[nCats]int64
	Dir      bool
	Cat      uint8
}

type scanState struct {
	root     string
	tree     *Node
	state    string // idle, scanning, done
	started  time.Time
	prevTime int64
	elapsed  float64
	files    atomic.Int64
	dirs     atomic.Int64
	bytes    atomic.Int64
	errs     atomic.Int64
	cloud    atomic.Int64
	current  atomic.Value
	trashed  int64
	sem      chan struct{}
	wg       sync.WaitGroup
}

var (
	mu  sync.RWMutex
	cur = &scanState{state: "idle"}
)

func (s *scanState) walk(path string, n *Node) {
	s.current.Store(path)
	f, err := os.Open(path)
	if err != nil {
		s.errs.Add(1)
		return
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil && len(entries) == 0 {
		s.errs.Add(1)
		return
	}
	n.Children = make([]*Node, 0, len(entries))
	var subdirs []*Node
	var localBytes, localFiles int64
	for _, e := range entries {
		name := e.Name()
		info, err := e.Info()
		if err != nil {
			continue
		}
		mode := info.Mode()
		if mode&fs.ModeSymlink != 0 {
			continue
		}
		if e.IsDir() {
			if isLinkDir(filepath.Join(path, name), info) {
				continue
			}
			c := &Node{Name: name, Dir: true, Mtime: info.ModTime().Unix()}
			n.Children = append(n.Children, c)
			subdirs = append(subdirs, c)
			continue
		}
		if mode&(fs.ModeDevice|fs.ModeNamedPipe|fs.ModeSocket|fs.ModeCharDevice) != 0 {
			continue
		}
		size := info.Size()
		if cloudOnly(info) {
			s.cloud.Add(size)
			continue
		}
		n.Children = append(n.Children, &Node{Name: name, Size: size, Mtime: info.ModTime().Unix(), Cat: catOf(name)})
		localBytes += size
		localFiles++
	}
	s.files.Add(localFiles)
	s.bytes.Add(localBytes)
	s.dirs.Add(1)
	for _, c := range subdirs {
		p := filepath.Join(path, c.Name)
		select {
		case s.sem <- struct{}{}:
			s.wg.Add(1)
			go func(p string, c *Node) {
				defer func() { <-s.sem; s.wg.Done() }()
				s.walk(p, c)
			}(p, c)
		default:
			s.walk(p, c)
		}
	}
}

func aggregate(n *Node) {
	if !n.Dir {
		return
	}
	var cats [nCats]int64
	n.Size, n.Files = 0, 0
	for _, c := range n.Children {
		if c.Dir {
			aggregate(c)
			n.Files += c.Files
			for i := range cats {
				cats[i] += c.Cats[i]
			}
		} else {
			n.Files++
			cats[c.Cat] += c.Size
		}
		n.Size += c.Size
	}
	n.Cats = &cats
	sort.Slice(n.Children, func(i, j int) bool { return n.Children[i].Size > n.Children[j].Size })
}

func startScan(root string) error {
	root = filepath.Clean(root)
	st, err := os.Stat(root)
	if err != nil || !st.IsDir() {
		return fmt.Errorf("not_folder")
	}
	mu.Lock()
	if cur.state == "scanning" {
		mu.Unlock()
		return fmt.Errorf("busy")
	}
	s := &scanState{root: root, state: "scanning", started: time.Now(), sem: make(chan struct{}, runtime.NumCPU()*2)}
	s.current.Store(root)
	cur = s
	mu.Unlock()
	go func() {
		t := &Node{Name: root, Dir: true}
		s.walk(root, t)
		s.wg.Wait()
		aggregate(t)
		applyHistory(s, t)
		mu.Lock()
		s.tree = t
		s.elapsed = time.Since(s.started).Seconds()
		s.state = "done"
		mu.Unlock()
	}()
	return nil
}

// findNode returns the node for a path and its chain of ancestors (root included).
func findNode(s *scanState, path string) (*Node, []*Node) {
	if s.tree == nil {
		return nil, nil
	}
	path = filepath.Clean(path)
	chain := []*Node{s.tree}
	if samePath(path, s.root) {
		return s.tree, chain
	}
	prefix := s.root
	if !strings.HasSuffix(prefix, string(os.PathSeparator)) {
		prefix += string(os.PathSeparator)
	}
	if len(path) <= len(prefix) || !samePath(path[:len(prefix)], prefix) {
		return nil, nil
	}
	n := s.tree
	for _, part := range strings.Split(path[len(prefix):], string(os.PathSeparator)) {
		var next *Node
		for _, c := range n.Children {
			if c.Name == part {
				next = c
				break
			}
		}
		if next == nil {
			return nil, nil
		}
		n = next
		chain = append(chain, n)
	}
	return n, chain
}

type outNode struct {
	N  string     `json:"n"`
	S  int64      `json:"s"`
	D  int        `json:"d,omitempty"`
	C  uint8      `json:"c"`
	F  int64      `json:"f,omitempty"`
	G  int64      `json:"g,omitempty"`
	M  int64      `json:"m,omitempty"`
	R  int        `json:"r,omitempty"` // number of grouped small items
	Ch []*outNode `json:"ch,omitempty"`
}

func prune(n *Node, min int64, depth int, budget *int) *outNode {
	o := &outNode{N: n.Name, S: n.Size, C: n.Cat, M: n.Mtime}
	if !n.Dir {
		return o
	}
	o.D, o.F = 1, n.Files
	if abs64(n.Delta) >= 10*mb {
		o.G = n.Delta
	}
	if depth <= 0 || *budget <= 0 {
		return o
	}
	var rest int64
	restN := 0
	for i, c := range n.Children {
		if c.Size < min || c.Size == 0 || *budget <= 0 {
			for _, r := range n.Children[i:] {
				rest += r.Size
			}
			restN = len(n.Children) - i
			break
		}
		*budget--
		o.Ch = append(o.Ch, prune(c, min, depth-1, budget))
	}
	if restN > 0 && rest > 0 {
		o.Ch = append(o.Ch, &outNode{S: rest, R: restN, C: 7})
	}
	return o
}

type nodeHeap []hItem
type hItem struct {
	n    *Node
	path string
}

func (h nodeHeap) Len() int            { return len(h) }
func (h nodeHeap) Less(i, j int) bool  { return h[i].n.Size > h[j].n.Size }
func (h nodeHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *nodeHeap) Push(x interface{}) { *h = append(*h, x.(hItem)) }
func (h *nodeHeap) Pop() interface{} {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

type topItem struct {
	P string `json:"p"`
	S int64  `json:"s"`
	C uint8  `json:"c"`
	M int64  `json:"m"`
}

func topFiles(n *Node, path string, cat int, limit int) []topItem {
	out := []topItem{}
	h := &nodeHeap{{n, path}}
	for h.Len() > 0 && len(out) < limit {
		it := heap.Pop(h).(hItem)
		if !it.n.Dir {
			out = append(out, topItem{it.path, it.n.Size, it.n.Cat, it.n.Mtime})
			continue
		}
		for _, c := range it.n.Children {
			if c.Size == 0 {
				break
			}
			if cat >= 0 {
				if c.Dir && c.Cats[cat] == 0 {
					continue
				}
				if !c.Dir && int(c.Cat) != cat {
					continue
				}
			}
			heap.Push(h, hItem{c, filepath.Join(it.path, c.Name)})
		}
	}
	return out
}

func removeNode(s *scanState, path string) {
	n, chain := findNode(s, path)
	if n == nil || len(chain) < 2 {
		return
	}
	parent := chain[len(chain)-2]
	for i, c := range parent.Children {
		if c == n {
			parent.Children = append(parent.Children[:i:i], parent.Children[i+1:]...)
			break
		}
	}
	var files int64 = 1
	var cats [nCats]int64
	if n.Dir {
		files, cats = n.Files, *n.Cats
	} else {
		cats[n.Cat] = n.Size
	}
	for _, a := range chain[:len(chain)-1] {
		a.Size -= n.Size
		if s.prevTime != 0 {
			a.Delta -= n.Size
		}
		a.Files -= files
		for i := range cats {
			a.Cats[i] -= cats[i]
		}
	}
	// Re-sort by size in every ancestor
	for i := 0; i < len(chain)-2; i++ {
		ch := chain[i].Children
		sort.Slice(ch, func(a, b int) bool { return ch[a].Size > ch[b].Size })
	}
	s.trashed += n.Size
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func failDetail(w http.ResponseWriter, code int, msg, detail string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg, "detail": detail})
}

const version = "1.2.0"

func main() {
	tb := make([]byte, 16)
	rand.Read(tb)
	token := hex.EncodeToString(tb)

	ln, err := net.Listen("tcp", "127.0.0.1:47600")
	if err != nil {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			fmt.Println("No se pudo iniciar / Could not start:", err)
			os.Exit(1)
		}
	}
	port := ln.Addr().(*net.TCPAddr).Port
	hosts := map[string]bool{fmt.Sprintf("127.0.0.1:%d", port): true, fmt.Sprintf("localhost:%d", port): true}

	mux := http.NewServeMux()
	api := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !hosts[r.Host] || r.Header.Get("X-Token") != token {
				fail(w, 403, "forbidden")
				return
			}
			h(w, r)
		})
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte(strings.NewReplacer("__TOKEN__", token, "__VERSION__", version).Replace(indexHTML)))
	})

	api("/api/places", func(w http.ResponseWriter, r *http.Request) {
		type folder struct {
			Name string `json:"name"`
			Path string `json:"path"`
		}
		folders := []folder{}
		if home, err := os.UserHomeDir(); err == nil {
			folders = append(folders, folder{"@home", home})
			for _, n := range []string{"Desktop", "Documents", "Downloads", "Videos", "Pictures", "Music", "OneDrive", "AppData"} {
				p := filepath.Join(home, n)
				if st, err := os.Stat(p); err == nil && st.IsDir() {
					folders = append(folders, folder{n, p})
				}
			}
		}
		writeJSON(w, map[string]interface{}{"drives": listDrives(), "folders": folders, "sep": string(os.PathSeparator)})
	})

	api("/api/list", func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Clean(r.URL.Query().Get("path"))
		entries, err := os.ReadDir(p)
		if err != nil {
			fail(w, 400, "unreadable")
			return
		}
		dirs := []string{}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e.Name())
			}
		}
		parent := filepath.Dir(p)
		if parent == p {
			parent = ""
		}
		writeJSON(w, map[string]interface{}{"path": p, "parent": parent, "dirs": dirs})
	})

	api("/api/scan", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Path string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil || body.Path == "" {
			fail(w, 400, "bad_request")
			return
		}
		if err := startScan(body.Path); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})

	api("/api/status", func(w http.ResponseWriter, r *http.Request) {
		mu.RLock()
		s := cur
		out := map[string]interface{}{
			"state": s.state, "root": s.root, "files": s.files.Load(), "dirs": s.dirs.Load(),
			"bytes": s.bytes.Load(), "errors": s.errs.Load(), "cloud": s.cloud.Load(),
			"elapsed": s.elapsed, "trashed": s.trashed, "sep": string(os.PathSeparator), "prevTime": s.prevTime,
		}
		if s.state == "scanning" {
			out["current"], _ = s.current.Load().(string)
			out["elapsed"] = time.Since(s.started).Seconds()
		}
		if s.state == "done" {
			out["bytes"] = s.tree.Size
			out["files"] = s.tree.Files
		}
		mu.RUnlock()
		if s.root != "" {
			total, free := diskSpace(s.root)
			out["diskTotal"], out["diskFree"] = total, free
		}
		writeJSON(w, out)
	})

	api("/api/tree", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.RLock()
		defer mu.RUnlock()
		n, _ := findNode(cur, q.Get("path"))
		if n == nil || !n.Dir {
			fail(w, 404, "gone")
			return
		}
		frac, _ := strconv.ParseFloat(q.Get("frac"), 64)
		if frac <= 0 || frac > 1 {
			frac = 0.0005
		}
		min := int64(float64(n.Size) * frac)
		budget := 15000
		t := prune(n, min, 14, &budget)
		t.N = filepath.Clean(q.Get("path"))
		writeJSON(w, map[string]interface{}{"tree": t, "cats": n.Cats, "root": cur.root})
	})

	api("/api/top", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		mu.RLock()
		defer mu.RUnlock()
		n, _ := findNode(cur, q.Get("path"))
		if n == nil {
			fail(w, 404, "not_found")
			return
		}
		cat := -1
		if c, err := strconv.Atoi(q.Get("cat")); err == nil && c >= 0 && c < nCats {
			cat = c
		}
		writeJSON(w, topFiles(n, filepath.Clean(q.Get("path")), cat, 100))
	})

	api("/api/changes", handleChanges)
	api("/api/cleanup", handleCleanup)
	api("/api/dups", handleDups)

	api("/api/reveal", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Path string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil {
			fail(w, 400, "bad_request")
			return
		}
		mu.RLock()
		n, _ := findNode(cur, body.Path)
		mu.RUnlock()
		if n == nil {
			fail(w, 404, "not_found")
			return
		}
		revealInExplorer(filepath.Clean(body.Path))
		writeJSON(w, map[string]bool{"ok": true})
	})

	api("/api/trash", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Path string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&body) != nil {
			fail(w, 400, "bad_request")
			return
		}
		p := filepath.Clean(body.Path)
		mu.Lock()
		defer mu.Unlock()
		n, chain := findNode(cur, p)
		if n == nil || len(chain) < 2 {
			fail(w, 400, "outside")
			return
		}
		if err := moveToTrash(p); err != nil {
			failDetail(w, 500, "trash_failed", err.Error())
			return
		}
		if _, err := os.Lstat(p); err == nil {
			fail(w, 500, "still_there")
			return
		}
		size := n.Size
		removeNode(cur, p)
		writeJSON(w, map[string]interface{}{"ok": true, "size": size})
	})

	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	fmt.Println()
	fmt.Println("  Mapa de Disco " + version)
	fmt.Println("  " + url)
	fmt.Println()
	fmt.Println("  Deja esta ventana abierta mientras lo uses. Para salir, cierra la ventana.")
	fmt.Println("  Keep this window open while you use it. Close it to quit.")
	if os.Getenv("MAPADISCO_NO_BROWSER") == "" {
		go func() { time.Sleep(300 * time.Millisecond); openBrowser(url) }()
	}
	http.Serve(ln, mux)
}
