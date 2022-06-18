package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
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
	"github.com/gabriel-vasile/mimetype"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/html"
	"github.com/tdewolff/minify/v2/js"
)

var (
	//go:embed "page.gohtml"
	pageTemplate string
	//go:embed "style.css"
	style    string
	page     *template.Template
	minifier *minify.M

	workDir string
	srcDir  string
	dstDir  string

	isRecursive  bool
	includeEmpty bool
	enableSort   bool
	convertLink  bool
	includeRegEx *regexp.Regexp
	excludeRegEx *regexp.Regexp
	baseURL      *url.URL

	linkMIME *mimetype.MIME
)

const (
	linkSuffix  = ".link"
	regularFile = os.FileMode(0666)
	defaultSrc  = "./"
	defaultDst  = "site"

	fuzzyFileName    = "fuzzy.json"
	metadataFileName = "statik.json"
)

type HTMLPayload struct {
	Parts      []Directory
	Root       Directory
	Stylesheet template.CSS
	Today      time.Time
}

type Directory struct {
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	SrcPath     string      `json:"-"`
	DstPath     string      `json:"-"`
	URL         *url.URL    `json:"url"`
	Directories []Directory `json:"directories"`
	Files       []File      `json:"files"`
	Size        string      `json:"size"`
	ModTime     time.Time   `json:"time"`
	Mode        fs.FileMode `json:"-"`
}

func (d *Directory) MarshalJSON() ([]byte, error) {
	type DirectoryAlias Directory
	return json.Marshal(&struct {
		URL string `json:"url"`
		*DirectoryAlias
	}{
		URL:            d.URL.String(),
		DirectoryAlias: (*DirectoryAlias)(d),
	})
}

type FuzzyFile struct {
	Name    string         `json:"name"`
	Path    string         `json:"path"`
	SrcPath string         `json:"-"`
	DstPath string         `json:"-"`
	URL     *url.URL       `json:"url"`
	MIME    *mimetype.MIME `json:"mime"`
	Mode    fs.FileMode    `json:"-"`
}

func (f *FuzzyFile) MarshalJSON() ([]byte, error) {
	type FuzzyFileAlias FuzzyFile
	return json.Marshal(&struct {
		URL  string `json:"url"`
		MIME string `json:"mime"`
		*FuzzyFileAlias
	}{
		URL:            f.URL.String(),
		MIME:           f.MIME.String(),
		FuzzyFileAlias: (*FuzzyFileAlias)(f),
	})
}

type File struct {
	FuzzyFile
	Size    string    `json:"size"`
	ModTime time.Time `json:"time"`
}

func (f *File) MarshalJSON() ([]byte, error) {
	type FileAlias File
	return json.Marshal(&struct {
		URL  string `json:"url"`
		MIME string `json:"mime"`
		*FileAlias
	}{
		URL:       f.FuzzyFile.URL.String(),
		MIME:      f.FuzzyFile.MIME.String(),
		FileAlias: (*FileAlias)(f),
	})
}

func (d Directory) isEmpty() bool { return len(d.Directories) == 0 && len(d.Files) == 0 }

// joins the baseURL with the given relative path in a new URL instance
func withBaseURL(rel string) (url *url.URL) {
	url, _ = url.Parse(baseURL.String()) // its clear that there can't be error here :)
	url.Path = path.Join(baseURL.Path, rel)
	return
}

func getAbsPath(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}

	return path.Join(workDir, rel)
}

func readIfNotEmpty(path string, dst *string) (err error) {
	var content []byte
	if path != "" {
		content, err = ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Could not read file: %s\n%s", path, err)
		}
		*dst = string(content)
	}
	return nil
}

func loadTemplate(name string, path string, buf *string) (tmpl *template.Template, err error) {
	if err = readIfNotEmpty(path, buf); err != nil {
		return
	}
	if tmpl, err = template.New(name).Parse(*buf); err != nil {
		return
	}
	return
}

func isDir(path string) (err error) {
	dir, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !dir.IsDir() {
		return fmt.Errorf("Expected %s to be a directory", path)
	}
	return nil
}

// The input path dir is assumed to be already absolute
func newFile(info os.FileInfo, dir string) (fz FuzzyFile, f File, err error) {
	if info.IsDir() {
		return fz, f, errors.New("newFile has been called with a os.FileInfo of type Directory")
	}

	var (
		rel, name, size string
		raw             []byte
		url             *url.URL
		mime            *mimetype.MIME
	)
	abs := path.Join(dir, info.Name())
	if rel, err = filepath.Rel(srcDir, abs); err != nil {
		return
	}

	url = withBaseURL(rel)
	size = humanize.Bytes(uint64(info.Size()))
	name = info.Name()
	if strings.HasSuffix(info.Name(), linkSuffix) {
		if raw, err = ioutil.ReadFile(abs); err != nil {
			return fz, f, fmt.Errorf("Could not read link file: %s\n%s\n", abs, err)
		}
		if url, err = url.Parse(strings.TrimSpace(string(raw))); err != nil {
			return fz, f, fmt.Errorf("Could not parse URL in file %s\n: %s\n%s\n", abs, raw, err)
		}

		size = humanize.Bytes(0)
		name = name[:len(name)-len(linkSuffix)]
		rel = rel[:len(rel)-len(linkSuffix)]
		mime = linkMIME
	} else if mime, err = mimetype.DetectFile(abs); err != nil {
		return
	}

	fz = FuzzyFile{
		Name:    name,
		Path:    rel,
		SrcPath: abs,
		DstPath: path.Join(dstDir, rel),
		URL:     url,
		MIME:    mime,
		Mode:    info.Mode(),
	}
	return fz, File{
		FuzzyFile: fz,
		Size:      size,
		ModTime:   info.ModTime(),
	}, nil
}

type Named interface {
	GetName() string
}

func (d Directory) GetName() string { return d.Name }
func (f File) GetName() string      { return f.FuzzyFile.Name }

func sortByName[T Named](infos []T) {
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].GetName() < infos[j].GetName()
	})
}

func includeDir(info fs.FileInfo) bool {
	return !excludeRegEx.MatchString(info.Name())
}

func includeFile(info fs.FileInfo) bool {
	return includeRegEx.MatchString(info.Name()) && !excludeRegEx.MatchString(info.Name())
}

func walk(base string) (dir Directory, fz []FuzzyFile, err error) {
	// Avoid infinite recursion over the destination directory
	if base == dstDir {
		return
	}

	var (
		infos   []fs.FileInfo
		dirInfo fs.FileInfo
		subdir  Directory
		subfz   []FuzzyFile
		file    File
		fuzzy   FuzzyFile
		rel     string
	)
	if infos, err = ioutil.ReadDir(base); err != nil {
		return dir, fz, fmt.Errorf("Could not read directory %s:\n%s", base, err)
	}

	if dirInfo, err = os.Stat(base); err != nil {
		return dir, fz, fmt.Errorf("Could not stat directory %s:\n%s", base, err)
	}

	if rel, err = filepath.Rel(srcDir, base); err != nil {
		return
	}

	// Avoid having the root named "." for estetich purpuses:
	// Extract an interesting name from the baseURL
	name := dirInfo.Name()
	if rel == "." && len(baseURL.Path) > 1 {
		parts := strings.Split(baseURL.Path, string(os.PathSeparator))
		name = parts[len(parts)-1]
	}

	dir = Directory{
		Name:    name,
		SrcPath: base,
		DstPath: path.Join(dstDir, rel),
		URL:     withBaseURL(rel),
		Path:    rel,
		Size:    humanize.Bytes(uint64(dirInfo.Size())),
		ModTime: dirInfo.ModTime(),
		Mode:    dirInfo.Mode(),
	}

	for _, info := range infos {
		if info.IsDir() && isRecursive && includeDir(info) {
			if subdir, subfz, err = walk(path.Join(base, info.Name())); err != nil {
				return
			}
			if !subdir.isEmpty() || includeEmpty {
				// include emptydir if isEmptyflag is setted
				dir.Directories = append(dir.Directories, subdir)
				fz = append(fz, subfz...)
			}
		} else if !info.IsDir() && includeFile(info) {
			if fuzzy, file, err = newFile(info, base); err != nil {
				return dir, fz, fmt.Errorf("Error while generating the File structure:\n%s", err)
			}
			fz = append(fz, fuzzy)
			dir.Files = append(dir.Files, file)
		}
	}
	if enableSort {
		sortByName(dir.Files)
		sortByName(dir.Directories)
	}
	return
}

func copy(f FuzzyFile) (err error) {
	var input []byte
	if input, err = ioutil.ReadFile(f.SrcPath); err != nil {
		return fmt.Errorf("Could not open %s for reading:\n%s", f.SrcPath, err)
	}
	if err = ioutil.WriteFile(f.DstPath, input, f.Mode); err != nil {
		return fmt.Errorf("Could not open %s for writing:\n%s", f.DstPath, err)
	}
	return nil
}

func writeCopies(dir Directory, fz []FuzzyFile) (err error) {
	dirs := append([]Directory{dir}, dir.Directories...)
	for len(dirs) != 0 {
		dirs = append(dirs, dirs[0].Directories...)
		if err = os.MkdirAll(dirs[0].DstPath, dirs[0].Mode); err != nil {
			return fmt.Errorf("Could not create output directory %s:\n%s", dirs[0].DstPath, err)
		}
		dirs = dirs[1:]
	}
	for _, f := range fz {
		if f.MIME == linkMIME {
			continue
		}
		if err = copy(f); err != nil {
			return err
		}
	}
	return nil
}

func jsonToFile[T any](path string, v T) (err error) {
	var data []byte
	if data, err = json.Marshal(&v); err != nil {
		return fmt.Errorf("Could not serialize JSON:\n%s", err)
	}
	if err = ioutil.WriteFile(path, data, regularFile); err != nil {
		return fmt.Errorf("Could not write metadata file %s:\n%s", path, err)
	}
	return nil
}

func writeJSON(dir Directory, fz []FuzzyFile) (err error) {
	// Write the fuzzy.json file in the root directory
	if len(fz) != 0 {
		if err = jsonToFile(path.Join(dir.DstPath, fuzzyFileName), fz); err != nil {
			return
		}
	}

	// Write the directory metadata
	if err = jsonToFile(path.Join(dir.DstPath, metadataFileName), dir); err != nil {
		return
	}

	for _, d := range dir.Directories {
		if err = writeJSON(d, []FuzzyFile{}); err != nil {
			return
		}
	}

	return nil
}

// Populates a HTMLPayload structure to generate an html listing file,
// propagating the generation recursively.
func writeHTML(dir Directory) (err error) {
	for _, d := range dir.Directories {
		if err = writeHTML(d); err != nil {
			return err
		}
	}

	var (
		index, relUrl string
		html          *os.File
	)

	index = path.Join(dir.DstPath, "index.html")
	if html, err = os.OpenFile(index, os.O_RDWR|os.O_CREATE, regularFile); err != nil {
		return fmt.Errorf("Could not create output file %s:\n%s\n", index, err)
	}
	defer html.Close()

	buf := new(bytes.Buffer)
	payload := HTMLPayload{
		Root:       dir,
		Stylesheet: template.CSS(style),
		Today:      time.Now(),
	}

	if dir.Path != "." {
		parts := strings.Split(dir.Path, string(os.PathSeparator))
		for _, part := range parts {
			relUrl = path.Join(relUrl, part)
			payload.Parts = append(payload.Parts, Directory{Name: part, URL: withBaseURL(relUrl)})
		}
		oldDirPath := strings.Join(parts[:len(parts)-1], string(os.PathListSeparator))
		oldDir := Directory{
			Name: "..",
			URL:  withBaseURL(oldDirPath),
		}
		payload.Root.Directories = append([]Directory{oldDir}, payload.Root.Directories...)
	}

	if err := page.Execute(buf, payload); err != nil {
		return fmt.Errorf("Could not generate listing template:\n%s", err)
	}

	if err = minifier.Minify("text/html", html, buf); err != nil {
		return fmt.Errorf("Could not minify page output:\n%s", err)
	}
	return nil
}

func logState() {
	log.Println("Running with parameters:")
	log.Println("\tInclude:\t", includeRegEx.String())
	log.Println("\tExclude:\t", excludeRegEx.String())
	log.Println("\tRecursive:\t", isRecursive)
	log.Println("\tEmpty:\t\t", includeEmpty)
	log.Println("\tConvert links:\t", convertLink)
	log.Println("\tSource:\t\t", srcDir)
	log.Println("\tDstination:\t", dstDir)
	log.Println("\tBase URL:\t", baseURL.String())
}

func sanitizeDirectories() (err error) {
	if strings.HasPrefix(srcDir, dstDir) {
		return errors.New("The output directory cannot be a parent of the input directory")
	}

	if _, err = os.OpenFile(srcDir, os.O_RDONLY, os.ModeDir|os.ModePerm); err != nil && os.IsPermission(err) {
		return fmt.Errorf("Cannot open source directory for reading: %s\n%s", srcDir, err)
	}

	if err := isDir(srcDir); err != nil {
		return err
	}

	// check if outputDir is writable
	if _, err = os.OpenFile(dstDir, os.O_WRONLY, os.ModeDir|os.ModePerm); err != nil && os.IsPermission(err) {
		return fmt.Errorf("Cannot open output directory for writing: %s\n%s", dstDir, err)
	}

	if err = os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("Cannot clear output directory: %s\n%s", dstDir, err)
	}
	return nil
}

func main() {
	var err error
	includeRegExStr := flag.String("i", ".*", "A regex pattern to include files into the listing")
	excludeRegExStr := flag.String("e", "\\.git(hub)?", "A regex pattern to exclude files from the listing")
	_isRecursive := flag.Bool("r", true, "Recursively scan the file tree")
	_includeEmpty := flag.Bool("empty", false, "Whether to list empty directories")
	_enableSort := flag.Bool("sort", true, "Sort files A-z and by type")
	rawURL := flag.String("b", "http://localhost", "The base URL")
	_convertLink := flag.Bool("l", false, "Convert .link files to anchor tags")
	pageTemplatePath := flag.String("page", "", "Use a custom listing page template")
	styleTemplatePath := flag.String("style", "", "Use a custom stylesheet file")
	targetHTML := flag.Bool("html", true, "Set false not to build html files (default true)")
	targetJSON := flag.Bool("json", true, "Set false not to build JSON metadata (default true)")
	flag.Parse()

	srcDir = defaultSrc
	dstDir = defaultDst
	isRecursive = *_isRecursive
	includeEmpty = *_includeEmpty
	enableSort = *_enableSort
	convertLink = *_convertLink

	args := flag.Args()
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "Usage: %s [dst] or [src] [dst]\n", os.Args[0])
		os.Exit(1)
	}
	if len(args) == 1 {
		dstDir = args[0]
	} else if len(args) == 2 {
		srcDir = args[0]
		dstDir = args[1]
	}

	if workDir, err = os.Getwd(); err != nil {
		log.Fatal("Could not get working directory", err)
	}

	// NOTA: in seguito queste funzioni di logging si possono mettere in if con una flag per verbose
	srcDir = getAbsPath(srcDir)
	dstDir = getAbsPath(dstDir)
	if err = sanitizeDirectories(); err != nil {
		log.Fatal("Error while checking src and dst paths", err)
	}

	if includeRegEx, err = regexp.Compile(*includeRegExStr); err != nil {
		log.Fatal("Invalid regexp for include matching", err)
	}
	if excludeRegEx, err = regexp.Compile(*excludeRegExStr); err != nil {
		log.Fatal("Invalid regexp for exclude matching", err)
	}

	if baseURL, err = url.Parse(*rawURL); err != nil {
		log.Fatal("Could not parse base URL", err)
	}
	logState()

	// Ugly hack to generate our custom mime, there currently is no way around this
	{
		v := true
		mimetype.Lookup("text/plain").Extend(func(_ []byte, size uint32) bool { return v }, "text/statik-link", ".link")
		linkMIME = mimetype.Detect([]byte("some plain text"))
		v = false
	}
	minifier = minify.New()
	minifier.AddFunc("text/css", css.Minify)
	minifier.AddFunc("text/html", html.Minify)
	minifier.AddFunc("application/javascript", js.Minify)

	if page, err = loadTemplate("page", *pageTemplatePath, &pageTemplate); err != nil {
		log.Fatal("Could not parse listing page template", err)
	}
	if err = readIfNotEmpty(*styleTemplatePath, &style); err != nil {
		log.Fatal("Could not read stylesheet file", err)
	}

	var (
		dir Directory
		fz  []FuzzyFile
	)
	if *targetHTML || *targetJSON {
		dir, fz, err = walk(srcDir)
		if err != nil {
			log.Fatalf("Error when creating the directory structure:\n%s\n", err)
		}

		if err = writeCopies(dir, fz); err != nil {
			log.Fatalf("Error while copying included files to the destination:\n%s\n", err)
		}
	}

	if *targetJSON {
		if err = writeJSON(dir, fz); err != nil {
			log.Fatalf("Error while generating JSON metadata:\n%s\n", err)
		}
	}

	if *targetHTML {
		if err = writeHTML(dir); err != nil {
			log.Fatalf("Error while generating HTML page listing:\n%s\n", err)
		}

	}
}
