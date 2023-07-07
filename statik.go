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
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
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
	Size        string      `json:"size"`
	ModTime     time.Time   `json:"time"`
	Mode        fs.FileMode `json:"-"`
	Directories []Directory `json:"directories,omitempty"`
	Files       []File      `json:"files,omitempty"`
	GenTime     time.Time   `json:"generated_at"`
}

func (d Directory) isEmpty() bool { return len(d.Directories) == 0 && len(d.Files) == 0 }

func (d *Directory) MarshalJSON() ([]byte, error) {
	type DirectoryAlias Directory
	return json.Marshal(&struct {
		URL     string `json:"url"`
		ModTime string `json:"time"`
		GenTime string `json:"generated_at"`
		*DirectoryAlias
	}{
		URL:            d.URL.String(),
		ModTime:        d.ModTime.Format(time.RFC3339),
		DirectoryAlias: (*DirectoryAlias)(d),
		GenTime:        d.GenTime.Format(time.RFC3339),
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
	// Unfortunately due to how go's embedding works, there is no other way
	// then to explicitly state all fields and reassign them
	return json.Marshal(&struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		URL     string `json:"url"`
		MIME    string `json:"mime"`
		Size    string `json:"size"`
		ModTime string `json:"time"`
	}{
		Name:    f.FuzzyFile.Name,
		Path:    f.FuzzyFile.Path,
		URL:     f.URL.String(),
		MIME:    f.MIME.String(),
		Size:    f.Size,
		ModTime: f.ModTime.Format(time.RFC3339),
	})
}

// Joins the baseURL with the given relative path in a new URL instance
func withBaseURL(rel string) (url *url.URL) {
	url, _ = url.Parse(baseURL.String())
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
		content, err = os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("could not read file: %s\n%s", path, err)
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

func requireDir(path string) (err error) {
	dir, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !dir.IsDir() {
		return fmt.Errorf("expected %s to be a directory", path)
	}
	return nil
}

// The input path dir is assumed to be already absolute
func newFile(entry os.DirEntry, dir string) (fz FuzzyFile, f File, err error) {
	if entry.IsDir() {
		return fz, f, errors.New("newFile has been called with a os.FileInfo of type Directory")
	}

	var (
		rel, name, size string
		raw             []byte
		url             *url.URL
		mime            *mimetype.MIME
	)
	abs := path.Join(dir, entry.Name())
	if rel, err = filepath.Rel(srcDir, abs); err != nil {
		return
	}

	url = withBaseURL(rel)
	info, err := os.Stat(abs)
	if err != nil {
		return
	}

	size = humanize.Bytes(uint64(info.Size()))
	name = entry.Name()
	if strings.HasSuffix(entry.Name(), linkSuffix) {
		if raw, err = os.ReadFile(abs); err != nil {
			return fz, f, fmt.Errorf("could not read link file: %s\n%w", abs, err)
		}
		if url, err = url.Parse(strings.TrimSpace(string(raw))); err != nil {
			return fz, f, fmt.Errorf("could not parse URL in file %s\n: %s\n%w", abs, raw, err)
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

func includeDir(info fs.DirEntry) bool {
	return !excludeRegEx.MatchString(info.Name())
}

func includeFile(info fs.DirEntry) bool {
	return includeRegEx.MatchString(info.Name()) && !excludeRegEx.MatchString(info.Name())
}

func walk(base string) (dir Directory, fz []FuzzyFile, err error) {
	// Avoid infinite recursion over the destination directory
	if base == dstDir {
		return
	}

	var (
		infos   []fs.DirEntry
		dirInfo fs.FileInfo
		subdir  Directory
		subfz   []FuzzyFile
		file    File
		fuzzy   FuzzyFile
		rel     string
	)
	if infos, err = os.ReadDir(base); err != nil {
		return dir, fz, fmt.Errorf("could not read directory %s:\n%s", base, err)
	}

	if dirInfo, err = os.Stat(base); err != nil {
		return dir, fz, fmt.Errorf("could not stat directory %s:\n%s", base, err)
	}

	if rel, err = filepath.Rel(srcDir, base); err != nil {
		return
	}

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
		GenTime: time.Now(),
	}

	for _, info := range infos {
		if info.IsDir() && isRecursive && includeDir(info) {
			if subdir, subfz, err = walk(path.Join(base, info.Name())); err != nil {
				return
			}
			if !subdir.isEmpty() || includeEmpty {
				// Include emptydir if isEmptyflag is setted
				dir.Directories = append(dir.Directories, subdir)
				fz = append(fz, subfz...)
			}
		} else if !info.IsDir() && includeFile(info) {
			if fuzzy, file, err = newFile(info, base); err != nil {
				return dir, fz, fmt.Errorf("error while generating the File structure:\n%s", err)
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

func copyFile(f FuzzyFile) (err error) {
	var input []byte
	if input, err = os.ReadFile(f.SrcPath); err != nil {
		return fmt.Errorf("could not open %s for reading:\n%s", f.SrcPath, err)
	}
	if err = os.WriteFile(f.DstPath, input, f.Mode); err != nil {
		return fmt.Errorf("could not open %s for writing:\n%s", f.DstPath, err)
	}
	return nil
}

func writeCopies(dir Directory, fz []FuzzyFile) (err error) {
	dirs := append([]Directory{dir}, dir.Directories...)
	for len(dirs) != 0 {
		dirs = append(dirs, dirs[0].Directories...)
		if err = os.MkdirAll(dirs[0].DstPath, dirs[0].Mode); err != nil {
			return fmt.Errorf("could not create output directory %s:\n%s", dirs[0].DstPath, err)
		}
		dirs = dirs[1:]
	}
	for _, f := range fz {
		if f.MIME == linkMIME {
			continue
		}
		if err = copyFile(f); err != nil {
			return err
		}
	}
	return nil
}

func jsonToFile[T any](path string, v T) (err error) {
	var data []byte
	if data, err = json.Marshal(&v); err != nil {
		return fmt.Errorf("could not serialize JSON:\n%s", err)
	}
	if err = os.WriteFile(path, data, regularFile); err != nil {
		return fmt.Errorf("could not write metadata file %s:\n%s", path, err)
	}
	return nil
}

// Create a shallow copy of a directory up to depth 2, meaning recursive
// directory listings are cleared but the directories in the current directory
// are maintained without stating their children files/directories
func shallow(dir Directory) Directory {
	cpy := dir
	cpy.Directories = make([]Directory, len(dir.Directories))
	copy(cpy.Directories, dir.Directories)
	for i := 0; i < len(cpy.Directories); i++ {
		cpy.Directories[i].Directories = nil
		cpy.Directories[i].Files = nil
	}
	return cpy
}

func writeJSON(dir *Directory, fz []FuzzyFile) (err error) {
	// Write the fuzzy.json file in the root directory
	if len(fz) != 0 {
		if err = jsonToFile(path.Join(dir.DstPath, fuzzyFileName), fz); err != nil {
			return
		}
	}

	// Write the directory metadata
	shallowCopy := shallow(*dir)
	if err = jsonToFile(path.Join(dir.DstPath, metadataFileName), &shallowCopy); err != nil {
		return
	}

	for _, d := range dir.Directories {
		if err = writeJSON(&d, []FuzzyFile{}); err != nil {
			return
		}
	}

	return nil
}

// Populates a HTMLPayload structure to generate an html listing file,
// propagating the generation recursively.
func writeHTML(dir *Directory) (err error) {
	for _, d := range dir.Directories {
		if err = writeHTML(&d); err != nil {
			return err
		}
	}

	var (
		index, relUrl string
		outputHtml    *os.File
	)

	index = path.Join(dir.DstPath, "index.html")
	if outputHtml, err = os.OpenFile(index, os.O_RDWR|os.O_CREATE, regularFile); err != nil {
		return fmt.Errorf("could not create output file %s:\n%s", index, err)
	}
	defer outputHtml.Close()

	buf := new(bytes.Buffer)
	payload := HTMLPayload{
		Root:       *dir,
		Stylesheet: template.CSS(style),
		Today:      dir.GenTime,
	}

	// Always append the last segment of the baseURL as a link back to the home
	payload.Parts = append(payload.Parts, Directory{
		Name: path.Base(baseURL.Path),
		URL:  baseURL,
	})
	if dir.Path != "." {
		parts := strings.Split(dir.Path, string(os.PathSeparator))
		for _, part := range parts {
			relUrl = path.Join(relUrl, part)
			payload.Parts = append(payload.Parts, Directory{Name: part, URL: withBaseURL(relUrl)})
		}

		back := path.Join(dir.Path, "..")
		payload.Root.Directories = append([]Directory{{
			Name: "..",
			Path: back,
			URL:  withBaseURL(back),
		}}, payload.Root.Directories...)
	}

	if err := page.Execute(buf, payload); err != nil {
		return fmt.Errorf("could not generate listing template:\n%s", err)
	}

	if err = minifier.Minify("text/html", outputHtml, buf); err != nil {
		return fmt.Errorf("could not minify page output:\n%s", err)
	}
	return nil
}

func sanitizeDirectories() (err error) {
	if strings.HasPrefix(srcDir, dstDir) {
		return errors.New("the output directory cannot be a parent of the input directory")
	}

	if _, err = os.OpenFile(srcDir, os.O_RDONLY, os.ModeDir|os.ModePerm); err != nil && os.IsPermission(err) {
		return fmt.Errorf("cannot open source directory for reading: %s\n%s", srcDir, err)
	}

	if err := requireDir(srcDir); err != nil {
		return err
	}

	// Check if outputDir is writable
	dir, err := os.OpenFile(dstDir, os.O_WRONLY, os.ModeDir|os.ModePerm)
	if err != nil && os.IsPermission(err) {
		return fmt.Errorf("cannot open output directory for writing: %s\n%s", dstDir, err)
	}
	defer dir.Close()

	if err = os.RemoveAll(dstDir); err != nil {
		return fmt.Errorf("cannot clear output directory: %s\n%s", dstDir, err)
	}
	return nil
}

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

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
	targetHTML := flag.Bool("html", true, "Set false not to build html files")
	targetJSON := flag.Bool("json", true, "Set false not to build JSON metadata")
	debug := flag.Bool("d", false, "Print debug logs")
	flag.Parse()

	if *debug {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}

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
	} else if len(args) == 1 {
		dstDir = args[0]
	} else if len(args) == 2 {
		srcDir = args[0]
		dstDir = args[1]
	} else {
		fmt.Fprintln(os.Stderr, "Invalid number of arguments, max 2 accepted")
		fmt.Fprintf(os.Stderr, "Usage: %s [-flags] [dst] or [src] [dst]\n", os.Args[0])
		os.Exit(1)
	}

	if workDir, err = os.Getwd(); err != nil {
		log.Fatal().Err(err).Msg("Could not get working directory")
	}

	srcDir = getAbsPath(srcDir)
	dstDir = getAbsPath(dstDir)
	if err = sanitizeDirectories(); err != nil {
		log.Fatal().Err(err).Msg("Error while checking src and dst paths")
	}

	if includeRegEx, err = regexp.Compile(*includeRegExStr); err != nil {
		log.Fatal().Err(err).Msg("Invalid regexp for include matching")
	}
	if excludeRegEx, err = regexp.Compile(*excludeRegExStr); err != nil {
		log.Fatal().Err(err).Msg("Invalid regexp for exclude matching")
	}

	if baseURL, err = url.Parse(*rawURL); err != nil {
		log.Fatal().Err(err).Msg("Could not parse base URL")
	}

	log.Print("Running with parameters:")
	log.Print("\tInclude:\t", includeRegEx.String())
	log.Print("\tExclude:\t", excludeRegEx.String())
	log.Print("\tRecursive:\t", isRecursive)
	log.Print("\tEmpty:\t\t", includeEmpty)
	log.Print("\tConvert links:\t", convertLink)
	log.Print("\tSource:\t\t", srcDir)
	log.Print("\tDstination:\t", dstDir)
	log.Print("\tBase URL:\t", baseURL.String())

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
		log.Fatal().Err(err).Msg("Could not parse listing page template")
	}
	if err = readIfNotEmpty(*styleTemplatePath, &style); err != nil {
		log.Fatal().Err(err).Msg("Could not read stylesheet file")
	}

	var (
		dir Directory
		fz  []FuzzyFile
	)
	if *targetHTML || *targetJSON {
		dir, fz, err = walk(srcDir)
		if err != nil {
			log.Fatal().Err(err).Msg("Error while walking the filesystem")
		}

		if err = writeCopies(dir, fz); err != nil {
			log.Fatal().Err(err).Msg("Error while copying included files to the destination")
		}
	}

	if *targetJSON {
		if err = writeJSON(&dir, fz); err != nil {
			log.Fatal().Err(err).Msg("Error while generating JSON metadata")
		}
	}

	if *targetHTML {
		if err = writeHTML(&dir); err != nil {
			log.Fatal().Err(err).Msg("Error while generating HTML page listing")
		}
	}
}
