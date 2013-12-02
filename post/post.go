// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package post

import (
	"bytes"
	"container/list"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html/template"
	"net/http"
	"path"
	"runtime/debug"
	"sort"
	"strings"
	"time"

	"code.google.com/p/rsc/appfs/fs"
	"code.google.com/p/rsc/appfs/proto"
	"code.google.com/p/rsc/blog/atom"

	ae "appengine"
	aeu "appengine/user"
)

// To find the PlusPage value of a Google Plus post:
// https://www.googleapis.com/plus/v1/people/{YOUR_PLUS_ID}/activities/public?key={YOUR_PLUS_KEY}

type Config struct {
	Name      string
	Email     string
	Account   string // Google accounts username
	PlusID    string // Google Plus ID of owner
	PlusKey   string // Google Plus Key of owner
	PublicURL string // Public URL of app web site
	FeedID    string
	FeedTitle string // Atom feed title
}

var config *Config

func Start(cfg *Config) {
	config = cfg
	http.HandleFunc("/", serve)
	http.Handle("/feeds/posts/default", http.RedirectHandler("/feed.atom", http.StatusFound))
}

var funcMap = template.FuncMap{
	"eq":     func(x, y string) bool { return x == y },
	"now":    time.Now,
	"date":   timeFormat,
	"join":   path.Join,
	"logged": func(user string) bool { return user != "?" && user != "" },
}

func timeFormat(fmt string, t time.Time) string {
	return t.Format(fmt)
}

type blogTime struct {
	time.Time
}

// Time formats, tried while parsing the Date field in a post
var timeFormats = []string{
	time.RFC3339,
	"Monday, January 2, 2006",
	"January 2, 2006 15:00 -0700",
}

func (t *blogTime) UnmarshalJSON(data []byte) (err error) {
	str := string(data)
	for _, f := range timeFormats {
		tt, err := time.Parse(`"`+f+`"`, str)
		if err == nil {
			t.Time = tt
			return nil
		}
	}
	return fmt.Errorf("did not recognize time: %s", str)
}

type PostData struct {
	FileModTime time.Time
	FileSize    int64

	Title    string
	Date     blogTime
	Name     string
	OldURL   string
	Summary  string
	Favorite bool
	NotInTOC bool
	Aux      string
	Author   string

	Reader []string

	PlusAuthor string // Google+ ID of author
	PlusPage   string // Google+ Post ID for comment post
	PlusAPIKey string // Google+ API key
	PlusURL    string
	HostURL    string // host URL
	Comments   bool

	article string
}

func (d *PostData) canRead(user string) bool {
	for _, r := range d.Reader {
		if r == user {
			return true
		}
	}
	return false
}

func (d *PostData) IsDraft() bool {
	return d.Date.IsZero() || d.Date.After(time.Now())
}

var replacer = strings.NewReplacer(
	"⁰", "<sup>0</sup>",
	"¹", "<sup>1</sup>",
	"²", "<sup>2</sup>",
	"³", "<sup>3</sup>",
	"⁴", "<sup>4</sup>",
	"⁵", "<sup>5</sup>",
	"⁶", "<sup>6</sup>",
	"⁷", "<sup>7</sup>",
	"⁸", "<sup>8</sup>",
	"⁹", "<sup>9</sup>",
	"ⁿ", "<sup>n</sup>",
	"₀", "<sub>0</sub>",
	"₁", "<sub>1</sub>",
	"₂", "<sub>2</sub>",
	"₃", "<sub>3</sub>",
	"₄", "<sub>4</sub>",
	"₅", "<sub>5</sub>",
	"₆", "<sub>6</sub>",
	"₇", "<sub>7</sub>",
	"₈", "<sub>8</sub>",
	"₉", "<sub>9</sub>",
	"``", "&ldquo;",
	"''", "&rdquo;",
)

func serve(w http.ResponseWriter, req *http.Request) {
	ctxt := fs.NewContext(req)
	ctxt.Criticalf("SERVING %s", req.URL.Path)

	// If a panic occurs in the user logic,
	// catch it, log it and return a 500 error.
	defer func() {
		if err := recover(); err != nil {
			var buf bytes.Buffer
			fmt.Fprintf(&buf, "panic: %s\n\n", err)
			buf.Write(debug.Stack())
			ctxt.Criticalf("%s", buf.String())

			http.Error(w, buf.String(), 500)
		}
	}()

	p := path.Clean("/" + req.URL.Path)

	// ☻ If the site is accessed via its appspot URL, redirect to the cutsom URL
	// to make sure links on the site are not broken.
	// if strings.Contains(req.Host, "appspot.com") {
	// 	http.Redirect(w, req, "http://research.swtch.com" + p, http.StatusFound)
	// }

	// ☻ Correct paths missing the root slash
	if p != req.URL.Path {
		http.Redirect(w, req, p, http.StatusFound)
		return
	}

	// ☻ Serve atom feed requests
	if p == "/feed.atom" {
		atomfeed(w, req)
		return
	}

	// ☻ Determine whether logged user is guest or owner
	user := ctxt.User()
	// isOwner = owner in AppEngine
	isOwner := aeu.IsAdmin(ae.NewContext(req)) || ctxt.User() == config.Account

	// ☻ If URL signifies the TOC page
	if p == "" || p == "/" || p == "/draft" {
		if p == "/draft" && user == "?" { // ☻ Prevent non-owners from viewing draft TOC pages
			ctxt.Criticalf("/draft loaded by %s", user)
			notfound(ctxt, w, req)
			return
		}
		toc(w, req, p == "/draft", isOwner, user) // Render
		return
	}

	// draft = we are in draft mode, and only if we have credentials
	draft := false
	if strings.HasPrefix(p, "/draft/") {
		if user == "?" {
			ctxt.Criticalf("/draft loaded by %s", user)
			notfound(ctxt, w, req)
			return
		}
		draft = true
		p = p[len("/draft"):]
	}

	/*
		// There are no valid URLs with slashes after the root or draft part of the URL.
		// We disable this, since we would like to be able to serve the whole MathJax tree statically.
		if strings.Contains(p[1:], "/") {
			notfound(ctxt, w, req)
			return
		}
	*/

	// If the path contains dots, it is interpreted as a static file
	if strings.Contains(p, ".") {
		// Let Google's front end servers cache static content for a short amount of time.
		// httpCache simply adds a caching directive in the HTTP response

		// Disable temporarily while fiddling with CSS files
		//httpCache(w, 5*time.Minute)
		ctxt.ServeFile(w, req, "blog/static/"+p)
		return
	}

	// Use just 'blog' as the cache path so that if we change
	// templates, all the cached HTML gets invalidated.
	var data []byte
	pp := "bloghtml:" + p
	if draft && !isOwner {
		pp += ",user=" + user
	}
	if key, ok := ctxt.CacheLoad(pp, "blog", &data); !ok {
		meta, article, err := loadPost(ctxt, p, req)
		if err != nil || meta.IsDraft() != draft || (draft && !isOwner && !meta.canRead(user)) {
			ctxt.Criticalf("no %s for %s", p, user)
			notfound(ctxt, w, req)
			return
		}
		t := mainTemplate(ctxt)
		template.Must(t.New("article").Parse(article))

		var buf bytes.Buffer
		meta.Comments = true
		if err := t.Execute(&buf, meta); err != nil {
			panic(err)
		}
		data = buf.Bytes()
		ctxt.CacheStore(key, data)
	}
	w.Write(data)
}

func notfound(ctxt *fs.Context, w http.ResponseWriter, req *http.Request) {
	var buf bytes.Buffer
	var data struct {
		HostURL string
	}
	data.HostURL = hostURL(req)
	t := mainTemplate(ctxt)
	if err := t.Lookup("404").Execute(&buf, &data); err != nil {
		panic(err)
	}
	w.WriteHeader(404)
	w.Write(buf.Bytes())
}

func mainTemplate(c *fs.Context) *template.Template {
	t := template.New("main")
	t.Funcs(funcMap)

	main, _, err := c.Read("blog/main.html")
	if err != nil {
		panic(err)
	}
	style, _, _ := c.Read("blog/style.html")
	main = append(main, style...)
	_, err = t.Parse(string(main))
	if err != nil {
		panic(err)
	}
	return t
}

// ☻ Parse a post file
func loadPost(c *fs.Context, name string, req *http.Request) (meta *PostData, article string, err error) {
	meta = &PostData{
		Name:       name,
		Title:      "¿Title?",
		PlusAuthor: config.PlusID,
		PlusAPIKey: config.PlusKey,
		HostURL:    hostURL(req),
	}

	art, fi, err := c.Read(name)
	if err != nil {
		return nil, "", err
	}
	if bytes.HasPrefix(art, []byte("{\n")) {
		i := bytes.Index(art, []byte("\n}\n"))
		if i < 0 {
			panic("cannot find end of json metadata")
		}
		hdr, rest := art[:i+3], art[i+3:]
		if err := json.Unmarshal(hdr, meta); err != nil {
			panic(fmt.Sprintf("loading %s: %s", name, err))
		}
		art = rest
	}
	meta.FileModTime = fi.ModTime
	meta.FileSize = fi.Size

	return meta, replacer.Replace(string(art)), nil
}

type byTime []*PostData

func (x byTime) Len() int           { return len(x) }
func (x byTime) Swap(i, j int)      { x[i], x[j] = x[j], x[i] }
func (x byTime) Less(i, j int) bool { return x[i].Date.Time.After(x[j].Date.Time) }

type TocData struct {
	User      string
	Draft     bool
	HostURL   string
	DraftRoot string // Base URL+path of draft articles
	PostRoot  string // Base URL+path of published articles
	Posts     []*PostData
}

// toc traverses the file system to build the list of posts
func toc(w http.ResponseWriter, req *http.Request, draft bool, isOwner bool, user string) {
	c := fs.NewContext(req)
	c.Criticalf("toc() draft=%v isOwner=%v user=%s", draft, isOwner, user)

	// ☻ Compute cache key for this page
	var data []byte
	keystr := fmt.Sprintf("blog:toc:%v", draft) // Key schema: "blog:toc:{true|false}" draft|non-draft
	if req.FormValue("readdir") != "" {
		keystr += ",readdir=" + req.FormValue("readdir") // If "readdir:" form value is given, add to cache key
	}
	if draft {
		keystr += ",user=" + user // If in draft mode, add user to cache key
	}

	// ☻ Try to load the page from the cache,
	if key, ok := c.CacheLoad(keystr, "blog", &data); ok {
		w.Write(data)
	} else {
		gentoc(w, req, key, draft, isOwner, user)
	}
}

func readDir(c *fs.Context, root string) ([]proto.FileInfo, error) {
	return c.ReadDir(root)
}

// readDirEllipses returns the file infos of all files descendent to root, and
// FileInfo.Name indicates the full file paths relative to root.
func readDirEllipses(c *fs.Context, root string) (r []proto.FileInfo, err error) {
	var q list.List // Queue of root-relative directory paths to recurse into
	q.PushBack(root)
	for e := q.Front(); e != nil; e = q.Front() {
		rpath := q.Remove(e).(string)
		if children, err := readDir(c, rpath); err != nil {
			return nil, err
		} else {
			for _, dir := range children {
				full := path.Join(rpath, dir.Name)
				if dir.IsDir {
					q.PushBack(full)
					continue
				}
				dir.Name = full // Substitute the name with complete path from root
				r = append(r, dir)
			}
		}
	}
	return
}

// ☻ Rebuild the TOC page, used on cache misses in toc.
func gentoc(w http.ResponseWriter, req *http.Request, key fs.CacheKey, draft, isOwner bool, user string) {
	var data []byte
	c := fs.NewContext(req)

	// ☻ Traverse "/blog/post/..." and its descendants
	dir, err := readDirEllipses(c, "blog/post")
	if err != nil {
		panic(err)
	}

	// ☻ If "readdir: 1" form field supplied, return number of files
	if req.FormValue("readdir") == "1" {
		fmt.Fprintf(w, "%d dir entries\n", len(dir))
		return
	}

	// ☻ Read postName–>postData from file "/blogcache", if any available
	postCache := map[string]*PostData{}
	if data, _, err := c.Read("blogcache"); err == nil {
		if err := json.Unmarshal(data, &postCache); err != nil {
			c.Criticalf("unmarshal blogcache: %v", err)
		}
	}

	ch := make(chan *PostData, len(dir)) // ☻ Create a channel whose buffer size equals the number of files in "blog/post"
	// XXX: This is a limiting mechanism. Use limiter.
	const par = 20
	var limit = make(chan bool, par) // Insert 20 tickets
	for i := 0; i < par; i++ {
		limit <- true
	}
	//
	for _, d := range dir { // For each file in directory,
		if meta := postCache[d.Name]; meta != nil && // Attempt to fetch post meta from "blogcache" file cache; if present, and
			meta.FileModTime.Equal(d.ModTime) && // The cache copy is not older than the original, and
			meta.FileSize == d.Size { // They match in size
			//
			ch <- meta // Use the cached post meta
			continue
		}

		<-limit
		go func(d proto.FileInfo) { // Fetch post in parallel
			defer func() { limit <- true }()
			meta, _, err := loadPost(c, d.Name, req)
			if err != nil {
				// Should not happen: we just listed the directory.
				c.Criticalf("loadPost %s: %v", d.Name, err)
				return
			}
			ch <- meta
		}(d)
	}
	for i := 0; i < par; i++ { // Wait for all post loads to complete
		<-limit
	}
	close(ch) // Write eof

	postCache = map[string]*PostData{} // ☻ Update postCache with the fresh data and apply permission/draft filters
	var all []*PostData
	for meta := range ch {
		postCache[meta.Name] = meta
		if (!draft && !meta.IsDraft() && !meta.NotInTOC) || (isOwner && draft) || meta.canRead(user) {
			all = append(all, meta)
		}
	}
	sort.Sort(byTime(all)) // ☻ Sort posts chronologically

	if data, err := json.Marshal(postCache); err != nil { // ☻ Write new TOC cache to "/blogcache"
		c.Criticalf("marshal blogcache: %v", err)
	} else if err := c.Write("blogcache", data); err != nil {
		c.Criticalf("write blogcache: %v", err)
	}

	var buf bytes.Buffer // ☻ Render TOC page
	t := mainTemplate(c)
	if err := t.Lookup("toc").Execute(&buf, &TocData{
		User:      c.User(),
		Draft:     draft,
		HostURL:   hostURL(req),
		DraftRoot: "/draft",
		PostRoot:  "/",
		Posts:     all,
	}); err != nil {
		panic(err)
	}
	data = buf.Bytes()
	c.CacheStore(key, data)
	//
	w.Write(data)
}

func hostURL(req *http.Request) string {
	if strings.Index(req.Host, "localhost") >= 0 {
		return "http://localhost:8000"
	}
	return config.PublicURL
}

func atomfeed(w http.ResponseWriter, req *http.Request) {
	c := fs.NewContext(req)

	c.Criticalf("Header: %v", req.Header)

	var data []byte
	if key, ok := c.CacheLoad("blog:atomfeed", "blog/post", &data); !ok {
		dir, err := c.ReadDir("blog/post")
		if err != nil {
			panic(err)
		}

		var all []*PostData
		for _, d := range dir {
			meta, article, err := loadPost(c, d.Name, req)
			if err != nil {
				// Should not happen: we just loaded the directory.
				panic(err)
			}
			if meta.IsDraft() {
				continue
			}
			meta.article = article
			all = append(all, meta)
		}
		sort.Sort(byTime(all))

		show := all
		if len(show) > 10 {
			show = show[:10]
			for _, meta := range all[10:] {
				if meta.Favorite {
					show = append(show, meta)
				}
			}
		}

		//
		//	Title
		//	ID
		//	Updated
		//	Author
		//		Name
		//		URI
		//		Email
		//	Link[]
		//		Rel
		//		Href
		feed := &atom.Feed{
			Title:   config.FeedTitle,
			ID:      config.FeedID,
			Updated: atom.Time(show[0].Date.Time),
			Author: &atom.Person{
				Name:  config.Name,
				URI:   "https://plus.google.com/" + config.PlusID,
				Email: config.Email,
			},
			Link: []atom.Link{
				{Rel: "self", Href: hostURL(req) + "/feed.atom"},
			},
		}

		for _, meta := range show {
			t := template.New("main")
			t.Funcs(funcMap)
			main, _, err := c.Read("blog/atom.html")
			if err != nil {
				panic(err)
			}
			_, err = t.Parse(string(main))
			if err != nil {
				panic(err)
			}
			template.Must(t.New("article").Parse(meta.article))
			var buf bytes.Buffer
			if err := t.Execute(&buf, meta); err != nil {
				panic(err)
			}

			e := &atom.Entry{
				Title: meta.Title,
				ID:    feed.ID + "/" + meta.Name,
				Link: []atom.Link{
					{Rel: "alternate", Href: meta.HostURL + "/" + meta.Name},
				},
				Published: atom.Time(meta.Date.Time),
				Updated:   atom.Time(meta.Date.Time),
				Summary: &atom.Text{
					Type: "text",
					Body: meta.Summary,
				},
				Content: &atom.Text{
					Type: "html",
					Body: buf.String(),
				},
			}

			feed.Entry = append(feed.Entry, e)
		}

		data, err = xml.Marshal(&feed)
		if err != nil {
			panic(err)
		}

		c.CacheStore(key, data)
	}

	// Feed readers like to hammer us; let Google cache the
	// response to reduce the traffic we have to serve.
	httpCache(w, 15*time.Minute)

	w.Header().Set("Content-Type", "application/atom+xml")
	w.Write(data)
}

func httpCache(w http.ResponseWriter, dt time.Duration) {
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(dt.Seconds())))
}
