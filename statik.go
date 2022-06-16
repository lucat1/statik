package main

import (
	"bytes"
	_ "embed"
	"errors"
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

// Describes the state of every main variable of the program
type ProgramState struct {
	isRecursive bool
	isEmpty     bool
	enableSort  bool
	convertLink bool

	styleTemplate  string
	footerTemplate string
	headerTemplate string
	lineTemplate   string

	srcDir  string
	destDir string

	includeRegEx    *regexp.Regexp
	excludeRegEx    *regexp.Regexp
	includeRegExStr string
	excludeRegExStr string
	URL             string
	baseURL         *url.URL
}

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
type Directory struct {
	Path        string      `json:"path"`
	Name        string      `json:"name"`
	Directories []Directory `json:"directories"`
	Files       []File      `json:"files"`
}

type FuzzyFile struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Mime string `json:"mime"`
}

type File struct {
	FuzzyFile
	Size    uint64    `json:"size"`
	ModTime time.Time `json:"time"`
}

// WARNING: don't call this with directory FileInfo, not supported
func getFile(file os.FileInfo, path string) File {
	return File{
		FuzzyFile: FuzzyFile{
			Path: path,
			Name: file.Name(),
			Mime: "tmp", // TODO: make a function that returns the correct mime
		},
		Size:    uint64(file.Size()),
		ModTime: file.ModTime(),
	}
}

// WARNING: don't call this with FileInfo that is not a directory, not supported
func getDirectory(file os.FileInfo, path string) Directory {
	return Directory{
		path,
		file.Name(),
		[]Directory{},
		[]File{},
	}
}

// Separates files and directories
func unpackFiles(fileInfo []os.FileInfo) ([]os.FileInfo, []os.FileInfo) {
	var files []os.FileInfo
	var dirs []os.FileInfo
	for _, file := range fileInfo {
		if !file.IsDir() {
			files = append(files, file)
		} else {
			dirs = append(dirs, file)
		}
	}
	return files, dirs
}

func IsPathValid(path string) error {
	dir, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !dir.IsDir() {
		return errors.New("the given path does not correspond to a directory")
	}
	return nil
}

func GetDirectoryStructure(path string, recursive bool, directory *Directory) error {
	err := IsPathValid(path)
	if err != nil {
		return err
	}

	// fill unitialized directory value (usually in first call)
	if directory.Name == "" {
		file, _ := os.Stat(path)
		*directory = getDirectory(file, path)
	}

	filesInDir, err := ioutil.ReadDir(path)
	if err != nil {
		return err
	}

	files, dirs := unpackFiles(filesInDir)
	for _, file := range files {
		directory.Files = append(directory.Files, getFile(file, path))
	}

	for _, dir := range dirs {
		directory.Directories = append(directory.Directories, getDirectory(dir, path))
	}

	if recursive {
		for idx, dir := range directory.Directories {
			dirName := filepath.Join(path, dir.Name)
			err := GetDirectoryStructure(dirName, true, &directory.Directories[idx])
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// used to handle the template programming
// every gohtml template should implement this interface
// TODO: this template interface could fit well with Registrary
// design pattern: everytime you have to use a new template
// you just register it!
type Template interface {
	Data(interface{}) interface{} // the interface that this template builds upon
	Load(string)                  // load the teplate
	Raw() string                  // the default filepath for the template
	Tmpl() *template.Template     // return the actual template
}

const linkSuffix = ".link"
const defaultSrc = "./"
const defaultDest = "site"

var (
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
func withBaseURL(state *ProgramState, rel string) string {
	cpy := state.baseURL.Path
	state.baseURL.Path = path.Join(state.baseURL.Path, rel)
	res := state.baseURL.String()
	state.baseURL.Path = cpy
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
		log.Fatalf("Could not open srcDir file for copying: %s\n%s\n", src, err)
	}
	err = ioutil.WriteFile(dest, input, 0644)
	if err != nil {
		log.Fatalf("Could not write to destination file for copying: %s\n%s\n", dest, err)
	}
}

func filter(state *ProgramState, entries []fs.FileInfo) []fs.FileInfo {
	filtered := []fs.FileInfo{}
	for _, entry := range entries {
		if entry.IsDir() && !state.excludeRegEx.MatchString(entry.Name()) ||
			(!entry.IsDir() && state.includeRegEx.MatchString(entry.Name()) &&
				!state.excludeRegEx.MatchString(entry.Name())) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// SECTION CLARITY: remove this later, just tmp for clarity
// sort by isDirectory and alphabetical naming
func sortAlphabetically(files []os.FileInfo) {
	sort.Slice(files, func(i, j int) bool {
		isFirstEntryDir := files[i].IsDir()
		isSecondEntryDir := files[j].IsDir()
		return isFirstEntryDir && !isSecondEntryDir ||
			(isFirstEntryDir || !isSecondEntryDir) &&
				files[i].Name() < files[j].Name()
	})
}

func generate(state *ProgramState, m *minify.M, dirName string, parts []string) bool {
	filesInDir, err := ioutil.ReadDir(dirName)
	if err != nil {
		log.Fatalf("Could not read input directory: %s\n%s\n", dirName, err)
	}

	filteredFiles := filter(state, filesInDir)
	if len(filteredFiles) == 0 {
		return state.isEmpty
	}

	if state.enableSort {
		sortAlphabetically(filteredFiles)
	}

	// CHECK IF OUTPUT DIRECTORY HAS GOOD PERMS
	rel := path.Join(parts...)
	outDir := path.Join(state.destDir, rel)
	err = os.Mkdir(outDir, os.ModePerm)
	if err != nil {
		log.Fatalf("Could not create output *sub*directory: %s\n%s\n", outDir, err)
	}

	// LOAD HTML
	htmlPath := path.Join(outDir, "index.html")
	html, err := os.OpenFile(htmlPath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("Could not create output index.html: %s\n%s\n", htmlPath, err)
	}

	out := new(bytes.Buffer)
	// TRACKER: variabili esterne usate:
	// parts
	// baseURL
	// Generate the header and the double dots back anchor when appropriate
	{
		p, url := []Dir{}, ""
		for _, part := range parts {
			url = path.Join(url, part)
			p = append(p, Dir{Name: part, URL: withBaseURL(state, url)})
		}
		gen(header, Header{
			Root: Dir{
				Name: strings.TrimPrefix(strings.TrimSuffix(state.baseURL.Path, "/"), "/"),
				URL:  state.baseURL.String(),
			},
			Parts:      p,
			FullPath:   path.Join(state.baseURL.Path+rel) + "/",
			Stylesheet: template.CSS(style),
		}, out)
	}

	// TRACKER: variabili esterne usate:
	// rel
	// Populate the back line
	{
		if len(parts) != 0 {
			gen(line, Line{
				IsDir: true,
				Name:  "..",
				URL:   withBaseURL(state, path.Join(rel, "..")),
				Size:  humanize.Bytes(0),
			}, out)
		}
	}

	// TRACKER: variabili esterne usate:
	// filteredFiles
	// rel
	// dirName
	// outDir
	// linkSuffix
	// line
	// state ProgramState
	for _, entry := range filteredFiles {
		pth := path.Join(dirName, entry.Name())
		// Avoid recursive infinite loop
		if pth == outDir {
			continue
		}

		data := Line{
			IsDir: entry.IsDir(),
			Name:  entry.Name(),
			URL:   withBaseURL(state, path.Join(rel, entry.Name())),
			Size:  humanize.Bytes(uint64(entry.Size())),
			Date:  entry.ModTime(),
		}

		if strings.HasSuffix(pth, linkSuffix) {
			data.Name = data.Name[:len(data.Name)-len(linkSuffix)] // get name without extension
			data.Size = humanize.Bytes(0)

			raw, err := ioutil.ReadFile(pth)
			if err != nil {
				log.Fatalf("Could not read link file: %s\n%s\n", pth, err)
			}
			rawStr := string(raw)
			u, err := url.Parse(strings.TrimSpace(rawStr))
			if err != nil {
				log.Fatalf("Could not parse URL in file: %s\nThe value is: %s\n%s\n", pth, raw, err)
			}

			data.URL = u.String()
			gen(line, data, out)
			continue
		}

		// TODO: simplify this logic
		// Only list directories when recursing and only those which are not empty
		if !entry.IsDir() || state.isRecursive && generate(state, m, pth, append(parts, entry.Name())) {
			gen(line, data, out)
		}

		// Copy all files over to the web root
		if !entry.IsDir() {
			copy(pth, path.Join(outDir, entry.Name()))
		}
	}
	gen(footer, Footer{Date: time.Now()}, out)
	err = m.Minify("text/html", html, out)
	if err != nil {
		log.Fatalf("Could not write to index.html: %s\n%s\n", htmlPath, err)
	}
	err = html.Close()
	if err != nil {
		log.Fatalf("Could not write to close index.html: %s\n%s\n", htmlPath, err)
	}
	log.Printf("Generated data for directory: %s\n", dirName)

	return !state.isEmpty
}

func loadTemplate(name string, path string, def *string, dest **template.Template) {
	var (
		content []byte
		err     error
	)
	if path != "" {
		content, err = ioutil.ReadFile(path)
		if err != nil {
			log.Fatalf("Could not read %s template file %s:\n%s\n", name, path, err)
		}
		*def = string(content)
	}
	*dest, err = template.New(name).Parse(*def)
	if err != nil {
		log.Fatalf("Could not parse %s template %s:\n%s\n", name, path, err)
	}
}

func logState(state *ProgramState) {
	log.Println("Running with parameters:")
	log.Println("\tInclude:\t", state.includeRegExStr)
	log.Println("\tExclude:\t", state.excludeRegExStr)
	log.Println("\tRecursive:\t", state.isRecursive)
	log.Println("\tEmpty:\t\t", state.isEmpty)
	log.Println("\tConvert links:\t", state.convertLink)
	log.Println("\tSource:\t\t", state.srcDir)
	log.Println("\tDestination:\t", state.destDir)
	log.Println("\tBase URL:\t", state.URL)
	log.Println("\tStyle:\t\t", state.styleTemplate)
	log.Println("\tFooter:\t\t", state.footerTemplate)
	log.Println("\tHeader:\t\t", state.headerTemplate)
	log.Println("\tline:\t\t", state.lineTemplate)
}

func getAbsolutePath(filePath string) string {
	if !filepath.IsAbs(filePath) {
		wd, err := os.Getwd()
		if err != nil {
			log.Fatal("Could not get currently working directory", err)
		}
		return path.Join(wd, filePath)
	} else {
		return filePath
	}
}

// remove all files from input directory
func clearDirectory(filePath string) {
	_, err := os.Stat(filePath)
	if err == nil {
		err = os.RemoveAll(filePath)
		if err != nil {
			log.Fatalf("Could not remove output directory previous contents: %s\n%s\n", filePath, err)
		}
	}
}

// handles every input parameter of the Program, returns it in ProgramState.
// if something its wrong, the whole program just panick-exits
func initProgram(state *ProgramState) {
	state.includeRegExStr = *flag.String("i", ".*", "A regex pattern to include files into the listing")
	state.excludeRegExStr = *flag.String("e", "\\.git(hub)?", "A regex pattern to exclude files from the listing")
	state.isRecursive = *flag.Bool("r", true, "Recursively scan the file tree")
	state.isEmpty = *flag.Bool("empty", false, "Whether to list empty directories")
	state.enableSort = *flag.Bool("sort", true, "Sort files A-z and by type")
	state.URL = *flag.String("b", "http://localhost", "The base URL")
	state.convertLink = *flag.Bool("l", false, "Convert .link files to anchor tags")
	state.styleTemplate = *flag.String("style", "", "Use a custom stylesheet file")
	state.footerTemplate = *flag.String("footer", "", "Use a custom footer template")
	state.headerTemplate = *flag.String("header", "", "Use a custom header template")
	state.lineTemplate = *flag.String("line", "", "Use a custom line template")
	state.srcDir = defaultSrc
	state.destDir = defaultDest
	flag.Parse()

	args := flag.Args()
	if len(args) > 2 {
		log.Fatal("Invalid number of arguments, expected two at max (source and dest)")
	}
	if len(args) == 1 {
		state.destDir = args[0]
	} else if len(args) == 2 {
		state.srcDir = args[0]
		state.destDir = args[1]
	}

	// NOTA: in seguito queste funzioni di logging si possono mettere in if con una flag per verbose
	logState(state)
	state.srcDir = getAbsolutePath(state.srcDir)
	state.destDir = getAbsolutePath(state.destDir)
	clearDirectory(state.destDir)

	var err error
	state.includeRegEx, err = regexp.Compile(state.includeRegExStr)
	if err != nil {
		log.Fatal("Invalid regexp for include matching", err)
	}
	state.excludeRegEx, err = regexp.Compile(state.excludeRegExStr)
	if err != nil {
		log.Fatal("Invalid regexp for exclude matching", err)
	}

	state.baseURL, err = url.Parse(state.URL)
	if err != nil {
		log.Fatalf("Could not parse base URL: %s\n%s\n", state.URL, err)
	}

	// TODO: use the registry design pattern to generalize the template loading, parsing and execution
	// This section should not belong to initProgram because it doesnt modify things on ProgramState,
	// just needs access
	loadTemplate("header", state.headerTemplate, &rawHeader, &header)
	loadTemplate("line", state.lineTemplate, &rawLine, &line)
	loadTemplate("footer", state.footerTemplate, &rawFooter, &footer)

	if state.styleTemplate != "" {
		var content []byte
		if content, err = ioutil.ReadFile(state.styleTemplate); err != nil {
			log.Fatalf("Could not read stylesheet file %s:\n%s\n", state.styleTemplate, err)
		}
		style = string(content)
	}
}

func main() {
	var state ProgramState
	var srcStructure Directory
	initProgram(&state)

	err := GetDirectoryStructure(state.srcDir, state.isRecursive, &srcStructure)
	if err != nil {
		log.Fatalf("Error when creating the directory structure:\n%s\n", err)
	}

	minifier := minify.New()
	minifier.AddFunc("text/css", css.Minify)
	minifier.AddFunc("text/html", html.Minify)
	minifier.AddFunc("application/javascript", js.Minify)
	generate(&state, minifier, state.srcDir, []string{})
}
