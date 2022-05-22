package main

import (
	"bytes"
	_ "embed"
	"flag"
	"html/template"
	"io"
	"io/fs"
	"io/ioutil"
	"log"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/html"
	"github.com/tdewolff/minify/v2/js"
)

type Dir struct {
	Name string
	URL  string
}

type Header struct {
	Root       Dir
	Parts      []Dir
	FullPath   string
	Stylesheet template.CSS
}

type Footer struct {
	Date time.Time
}

type Line struct {
	IsDir bool
	Name  string
	URL   string
	Size  string
	Date  time.Time
}

const linkSuffix = ".link"

var (
	baseDir, outDir string
	baseURL         *url.URL = nil

	include, exclude                           *regexp.Regexp = nil, nil
	empty, recursive, sortEntries, converLinks bool

	//go:embed "style.css"
	style string
	//go:embed "header.gohtml"
	rawHeader string
	//go:embed "line.gohtml"
	rawLine string
	//go:embed "footer.gohtml"
	rawFooter string

	header, footer, line *template.Template
)

// joins the baseUrl path with the given relative path and returns the url as a string
func withBaseURL(rel string) string {
	cpy := baseURL.Path
	baseURL.Path = path.Join(baseURL.Path, rel)
	res := baseURL.String()
	baseURL.Path = cpy
	return res
}

func gen(tmpl *template.Template, data interface{}, out io.Writer) {
	if err := tmpl.Execute(out, data); err != nil {
		log.Fatalf("could not generate template for the %s section:\n%s\n", tmpl.Name(), err)
	}
}

func copy(src, dest string) {
	input, err := ioutil.ReadFile(src)
	if err != nil {
		log.Fatalf("Could not open source file for copying: %s\n%s\n", src, err)
	}
	err = ioutil.WriteFile(dest, input, 0644)
	if err != nil {
		log.Fatalf("Could not write to destination file for copying: %s\n%s\n", dest, err)
	}
}

func filter(entries []fs.FileInfo) []fs.FileInfo {
	filtered := []fs.FileInfo{}
	for _, entry := range entries {
		if entry.IsDir() && !exclude.MatchString(entry.Name()) || (!entry.IsDir() && include.MatchString(entry.Name()) && !exclude.MatchString(entry.Name())) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func generate(m *minify.M, dir string, parts []string) bool {
	entries, err := ioutil.ReadDir(dir)
	if err != nil {
		log.Fatalf("Could not read input directory: %s\n%s\n", dir, err)
	}
	entries = filter(entries)
	if len(entries) == 0 {
		return empty
	}
	if sortEntries {
		sort.Slice(entries, func(i, j int) bool {
			isFirstEntryDir := entries[i].IsDir()
			isSecondEntryDir := entries[j].IsDir()
			return isFirstEntryDir && !isSecondEntryDir ||
				(isFirstEntryDir || !isSecondEntryDir) &&
					entries[i].Name() < entries[j].Name()
		})
	}

	rel := path.Join(parts...)
	outDir := path.Join(outDir, rel)
	if err := os.Mkdir(outDir, os.ModePerm); err != nil {
		log.Fatalf("Could not create output *sub*directory: %s\n%s\n", outDir, err)
	}
	htmlPath := path.Join(outDir, "index.html")
	html, err := os.OpenFile(htmlPath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("Could not create output index.html: %s\n%s\n", htmlPath, err)
	}

	out := new(bytes.Buffer)
	// Generate the header and the double dots back anchor when appropriate
	{
		p, url := []Dir{}, ""
		for _, part := range parts {
			url = path.Join(url, part)
			p = append(p, Dir{Name: part, URL: withBaseURL(url)})
		}
		gen(header, Header{
			Root: Dir{
				Name: strings.TrimPrefix(strings.TrimSuffix(baseURL.Path, "/"), "/"),
				URL:  baseURL.String(),
			},
			Parts:      p,
			FullPath:   path.Join(baseURL.Path+rel) + "/",
			Stylesheet: template.CSS(style),
		}, out)
	}
	// Populte the back line
	{
		if len(parts) != 0 {
			gen(line, Line{
				IsDir: true,
				Name:  "..",
				URL:   withBaseURL(path.Join(rel, "..")),
				Size:  humanize.Bytes(0),
			}, out)
		}
	}

	for _, entry := range entries {
		pth := path.Join(dir, entry.Name())
		// Avoid recursive infinite loop
		if pth == outDir {
			continue
		}

		data := Line{
			IsDir: entry.IsDir(),
			Name:  entry.Name(),
			URL:   withBaseURL(path.Join(rel, entry.Name())),
			Size:  humanize.Bytes(uint64(entry.Size())),
			Date:  entry.ModTime(),
		}
		if strings.HasSuffix(pth, linkSuffix) {
			data.Name = data.Name[:len(data.Name)-len(linkSuffix)]
			data.Size = humanize.Bytes(0)

			raw, err := ioutil.ReadFile(pth)
			if err != nil {
				log.Fatalf("Could not read link file: %s\n%s\n", pth, err)
			}
			rawStr := string(raw)
			u, err := url.Parse(rawStr[:len(rawStr)-1])
			if err != nil {
				log.Fatalf("Could not parse URL in file: %s\nThe value is: %s\n%s\n", pth, raw, err)
			}

			data.URL = u.String()
			gen(line, data, out)
			continue
		}

		// Only list directories when recursing and only those which are not empty
		if !entry.IsDir() || recursive && generate(m, pth, append(parts, entry.Name())) {
			gen(line, data, out)
		}

		// Copy all files over to the web root
		if !entry.IsDir() {
			copy(pth, path.Join(outDir, entry.Name()))
		}
	}
	gen(footer, Footer{Date: time.Now()}, out)
	if err := m.Minify("text/html", html, out); err != nil {
		log.Fatalf("Could not write to index.html: %s\n%s\n", htmlPath, err)
	}
	if err := html.Close(); err != nil {
		log.Fatalf("Could not write to close index.html: %s\n%s\n", htmlPath, err)
	}
	log.Printf("Generated data for directory: %s\n", dir)

	return !empty
}

func loadTemplate(name string, path string, def *string, dest **template.Template) {
	var (
		content []byte
		err     error
	)
	if path != "" {
		if content, err = ioutil.ReadFile(path); err != nil {
			log.Fatalf("Could not read %s template file %s:\n%s\n", name, path, err)
		}
		*def = string(content)
	}
	if *dest, err = template.New(name).Parse(*def); err != nil {
		log.Fatalf("Could not parse %s template:\n%s\n", name, path, err)
	}
}

func main() {
	i := flag.String("i", ".*", "A regex pattern to include files into the listing")
	e := flag.String("e", "\\.git(hub)?", "A regex pattern to exclude files from the listing")
	r := flag.Bool("r", true, "Recursively scan the file tree")
	emp := flag.Bool("empty", false, "Whether to list empty directories")
	s := flag.Bool("sort", true, "Sort files A-z and by type")
	b := flag.String("b", "http://localhost", "The base URL")
	l := flag.Bool("l", false, "Convert .link files to anchor tags")
	argstyle := flag.String("style", "", "Use a custom stylesheet file")
	argfooter := flag.String("footer", "", "Use a custom footer template")
	argheader := flag.String("header", "", "Use a custom header template")
	argline := flag.String("line", "", "Use a custom line template")
	flag.Parse()

	args := flag.Args()
	src, dest := ".", "site"
	if len(args) > 2 {
		log.Fatal("Invalid number of aruments, expected two at max")
	}
	if len(args) == 1 {
		dest = args[0]
	} else if len(args) == 2 {
		src = args[0]
		dest = args[1]
	}
	log.Println("Running with parameters:")
	log.Println("\tInclude:\t", *i)
	log.Println("\tExclude:\t", *e)
	log.Println("\tRecursive:\t", *r)
	log.Println("\tEmpty:\t\t", *emp)
	log.Println("\tConvert links:\t", *l)
	log.Println("\tSource:\t\t", src)
	log.Println("\tDestination:\t", dest)
	log.Println("\tBase URL:\t", *b)
	log.Println("\tStyle:\t\t", *argstyle)
	log.Println("\tFooter:\t\t", *argfooter)
	log.Println("\tHeader:\t\t", *argheader)
	log.Println("\tline:\t\t", *argline)

	var err error
	if include, err = regexp.Compile(*i); err != nil {
		log.Fatal("Invalid regexp for include matching", err)
	}
	if exclude, err = regexp.Compile(*e); err != nil {
		log.Fatal("Invalid regexp for exclude matching", err)
	}
	recursive = *r
	empty = *emp
	sortEntries = *s
	converLinks = *l

	var wd string
	if !filepath.IsAbs(src) || !filepath.IsAbs(dest) {
		wd, err = os.Getwd()
		if err != nil {
			log.Fatal("Could not get currently working directory", err)
		}
	}
	if baseDir = src; !filepath.IsAbs(src) {
		baseDir = path.Join(wd, src)
	}
	if outDir = dest; !filepath.IsAbs(dest) {
		outDir = path.Join(wd, dest)
	}
	if _, err := os.Stat(outDir); err == nil {
		if err = os.RemoveAll(outDir); err != nil {
			log.Fatalf("Could not remove output directory previous contents: %s\n%s\n", outDir, err)
		}
	}
	if baseURL, err = url.Parse(*b); err != nil {
		log.Fatalf("Could not parse base URL: %s\n%s\n", *b, err)
	}

	loadTemplate("header", *argheader, &rawHeader, &header)
	loadTemplate("line", *argline, &rawLine, &line)
	loadTemplate("footer", *argfooter, &rawFooter, &footer)

	if *argstyle != "" {
		var content []byte
		if content, err = ioutil.ReadFile(*argstyle); err != nil {
			log.Fatalf("Could not read stylesheet file %s:\n%s\n", *argstyle, err)
		}
		style = string(content)
	}

	m := minify.New()
	m.AddFunc("text/css", css.Minify)
	m.AddFunc("text/html", html.Minify)
	m.AddFunc("application/javascript", js.Minify)
	generate(m, baseDir, []string{})
}
