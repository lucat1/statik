package main

import (
	"flag"
	"fmt"
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
	_"embed"
	"html/template"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
)

type Header struct {
	Path string
	Mystyle template.CSS
}

type Footer struct {
	Date string
}

type Dotdot struct {
	Path string
}

type Line struct {
	Url 	string
	Name 	string
	Time 	string
	Size 	string
	Extra 	string
}

const (
	formatLayout                                       = time.RFC822
	linkSuffix                                         = ".link"
)

var (
	baseDir, outDir string
	baseUrl         *url.URL = nil

	include, exclude                           *regexp.Regexp = nil, nil
	empty, recursive, sortEntries, converLinks bool

	//go:embed "default-components/style.css"
	styleFile string
	//go:embed "default-components/header.gohtml"
	headerFile string
	//go:embed "default-components/footer.gohtml"
	footerFile string
	//go:embed "default-components/dotdot.gohtml"
	dotdotFile string
	//go:embed "default-components/line.gohtml"
	lineFile string

	headerTemplate	*template.Template
	footerTemplate	*template.Template
	dotdotTemplate	*template.Template
	lineTemplate 	*template.Template
)

func bytes(b int64) string {
	const unit = 1000
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "kMGTPE"[exp])
}

// joins the baseUrl path with the given relative path and returns the url as a string
func join(rel string) string {
	cpy := baseUrl.Path
	baseUrl.Path = path.Join(baseUrl.Path, rel)
	res := baseUrl.String()
	baseUrl.Path = cpy
	return res
}

func header(rel string) string {
	path := path.Join(baseUrl.Path + rel)
	var out strings.Builder
	h := Header{
		Path:path, 
		Mystyle:template.CSS(styleFile)}
	if err := headerTemplate.Execute(&out, h); err != nil {
		log.Fatalf("could not generate header lines for path: %s", path)
	}
	if rel != "/" {
		d := Dotdot{join(rel+"/..")}
		if err := dotdotTemplate.Execute(&out, d); err != nil {
			log.Fatalf("could not generate dotdot line for path: %s", path)
		}
		
	}
	return out.String()
}

func line(name string, path string, modTime time.Time, isDir bool, size int64, link bool) string {
	url := path
	var out strings.Builder
	if !link {
		url = join(path)
	}
	extra := ""
	if isDir {
		extra = "class=\"d\""
	}

	l := Line{url, name, modTime.Format(formatLayout), bytes(size), extra}
	if err := lineTemplate.Execute(&out, l); err != nil {
		log.Fatalf("could not generate line template on path: %s", path)
	}
	return out.String()
}

func footer(date time.Time) string {
	var out strings.Builder
	f := Footer{date.Format(formatLayout)}
	if err := footerTemplate.Execute(&out, f); err != nil {
		log.Fatalf("could not generate footer template")
	}
	return out.String()
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

func generate(dir string) bool {
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

	if !strings.HasSuffix(dir, "/") {
		dir += "/"
	}
	rel := strings.Replace(dir, baseDir, "", 1)
	out := path.Join(outDir, rel)
	if err := os.Mkdir(out, os.ModePerm); err != nil {
		log.Fatalf("Could not create output *sub*directory: %s\n%s\n", out, err)
	}
	htmlPath := path.Join(out, "index.html")
	html, err := os.OpenFile(htmlPath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("Could not create output index.html: %s\n%s\n", htmlPath, err)
	}

	content := header(rel)
	for _, entry := range entries {
		pth := path.Join(dir, entry.Name())
		// Avoid recursive infinite loop
		if pth == outDir {
			continue
		}

		if strings.HasSuffix(pth, linkSuffix) {
			url, err := ioutil.ReadFile(pth)
			if err != nil {
				log.Fatalf("Could not read link file: %s\n%s\n", pth, err)
			}
			content += line(entry.Name()[:len(entry.Name())-len(linkSuffix)], string(url), entry.ModTime(), entry.IsDir(), 0, true)
			continue
		}

		// Only list directories when recursing and only those which are not empty
		if !entry.IsDir() || recursive && generate(pth) {
			content += line(entry.Name(), path.Join(rel, entry.Name()), entry.ModTime(), entry.IsDir(), entry.Size(), false)
		}

		// Copy all files over to the web root
		if !entry.IsDir() {
			copy(pth, path.Join(out, entry.Name()))
		}
	}
	content += footer(time.Now())
	if n, err := html.Write([]byte(content)); err != nil || n != len(content) {
		log.Fatalf("Could not write to index.html: %s\n%s\n", htmlPath, err)
	}
	if err := html.Close(); err != nil {
		log.Fatalf("Could not write to close index.html: %s\n%s\n", htmlPath, err)
	}
	log.Printf("Generated data for directory: %s\n", dir)

	return !empty
}

func main() {
	i := flag.String("i", ".*", "A regex pattern to include files into the listing")
	e := flag.String("e", "\\.git(hub)?", "A regex pattern to exclude files from the listing")
	r := flag.Bool("r", true, "Recursively scan the file tree")
	emp := flag.Bool("empty", false, "Whether to list empty directories")
	s := flag.Bool("sort", true, "Sort files A-z and by type")
	b := flag.String("b", "http://localhost", "The base URL")
	l := flag.Bool("l", false, "Convert .link files to anchor tags")
	argstyle := flag.String("style", "", "Add a custom style file gohtml")
	argfooter := flag.String("footer", "", "Add a custom footer gohtml")
	argheader := flag.String("header", "", "Add a custom header gohtml")
	argdotdot := flag.String("dotdot", "", "Add a custom dotdot line gohtml")
	argline := flag.String("line", "", "Add a custom line gohtml")
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
	log.Println("\tConvert links:\t\t", *l)
	log.Println("\tSource:\t\t", src)
	log.Println("\tDestination:\t", dest)
	log.Println("\tBase URL:\t", *b)
	log.Println("\tstyle:\t\t",*argstyle)
	log.Println("\tfooter:\t\t",*argfooter)
	log.Println("\theader:\t\t",*argheader)
	log.Println("\tdotdot:\t\t",*argdotdot)
	log.Println("\tline:\t\t",*argline)

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
	if baseUrl, err = url.Parse(*b); err != nil {
		log.Fatalf("Could not parse base URL: %s\n%s\n", *b, err)
	}
	var content []byte
	
	if *argheader != "" { 
		if content, err = ioutil.ReadFile(*argheader); err != nil {
			log.Fatalf("Could not open header file template")
		}
		headerFile = string(content)
	}
	if headerTemplate, err = template.New("header").Parse(headerFile); err != nil { log.Fatalf("could not create header template")}
	
	if *argfooter != "" {
		if content, err = ioutil.ReadFile(*argfooter); err != nil {
			log.Fatalf("Could not open header file template")
		} 
		footerFile = string(content)
	}
	if footerTemplate, err = template.New("footer").Parse(footerFile); err != nil { log.Fatalf("could not create footer template")}
	
	if *argdotdot != "" { 
		if content, err = ioutil.ReadFile(*argdotdot); err != nil {
			log.Fatalf("Could not open header file template")
		}
		dotdotFile = string(content)
	}
	if dotdotTemplate, err = template.New("dotdot").Parse(dotdotFile); err != nil { log.Fatalf("could not create dotdot template")}
	
	if *argline != "" { 
		if content, err = ioutil.ReadFile(*argline); err != nil {
			log.Fatalf("Could not open header file template")
		}
		lineFile = string(content)
	}
	if lineTemplate, err = template.New("line").Parse(lineFile); err != nil { log.Fatalf("could not create line template")}
	
	if *argstyle != "" {
		if content, err = ioutil.ReadFile(*argstyle); err != nil {
			log.Fatalf("Could not open header file template")
		}
		styleFile = string(content)
	}

	log.Printf("templates created correctly")
	
	m := minify.New()
	m.AddFunc("text/css", css.Minify)
	if styleFile, err = m.String("text/css", styleFile); err != nil {
		log.Fatalf("Could not minify css")
	}
	log.Printf("css code minified correctly")
	
	generate(baseDir)
}
