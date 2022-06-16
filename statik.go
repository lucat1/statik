package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"html/template"
	"io"
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
	IsRecursive bool
	IsEmpty     bool
	EnableSort  bool
	ConvertLink bool

	// TODO: i have to convert these template string in other form, so that i can generalize the
	// generation
	StyleTemplate  string
	FooterTemplate string
	HeaderTemplate string
	LineTemplate   string

	SrcDir  string
	DestDir string

	IncludeRegEx    *regexp.Regexp
	ExcludeRegEx    *regexp.Regexp
	IncludeRegExStr string
	ExcludeRegExStr string
	URL             string
	BaseURL         *url.URL

	Minifier *minify.M
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

// this interface will be used to handle the template programming
// every gohtml template should implement this interface
// TODO: this template interface could fit well with Registrary
// design pattern: everytime you have to use a new template
// you just register it (aka implement needed functions)!
// PROBLEM: i don't have any idea how to make it, i don't even know
// if it could be a good choice
type Template interface {
	Data(interface{}) interface{} // the interface that this template builds upon
	Load(string)                  // load the teplate
	Raw() string                  // the default filepath for the template
	Tmpl() *template.Template     // just return the template pointer
}

type Directory struct {
	Path        string      `json:"path"`
	Name        string      `json:"name"`
	Directories []Directory `json:"directories"`
	Files       []File      `json:"files"`
	Size        uint64      `json:"size"`
	ModTime     time.Time   `json:"time"`
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

const linkSuffix = ".link"
const defaultSrc = "./"
const defaultDest = "site"

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
		Path:        path,
		Name:        file.Name(),
		Directories: []Directory{},
		Files:       []File{},
		Size:        uint64(file.Size()),
		ModTime:     file.ModTime(),
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

// joins the BaseUrl path with the given relative path and returns the url as a string
func withBaseURL(state *ProgramState, rel string) string {
	cpy := state.BaseURL.Path
	state.BaseURL.Path = path.Join(state.BaseURL.Path, rel)
	res := state.BaseURL.String()
	state.BaseURL.Path = cpy
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
		log.Fatalf("Could not open SrcDir file for copying: %s\n%s\n", src, err)
	}
	err = ioutil.WriteFile(dest, input, 0644)
	if err != nil {
		log.Fatalf("Could not write to destination file for copying: %s\n%s\n", dest, err)
	}
}

// NOTA: avevo bisogno di una funzione che filtri sia Directory che Files
// Non sono riuscito in breve a creare tale cosa: (dovrebbe avere in input un interfaccia
// che generalizzi il Name per directory e Files, e avere una funzione in input che dica come
// filtrare)
func filterDirs(state *ProgramState, entries []Directory) []Directory {
	filtered := []Directory{}
	for _, entry := range entries {
		if !state.ExcludeRegEx.MatchString(entry.Name) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// VEDI NOTA filterDirs
func filterFiles(state *ProgramState, entries []File) []File {
	filtered := []File{}
	for _, entry := range entries {
		if state.IncludeRegEx.MatchString(entry.Name) && !state.ExcludeRegEx.MatchString(entry.Name) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// FIXME: i have to sort both Directories and Files, need a way to make
// them both at once
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

// REGION GENERATE
// TODO: these functions should be later generalized with interface and so on...
// the function parameters are temporary, i have to find a way to reduce it...

// Generate the header and the double dots back anchor when appropriate
func generateHeader(state *ProgramState, parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	p, url := []Dir{}, ""
	for _, part := range parts {
		url = path.Join(url, part)
		p = append(p, Dir{Name: part, URL: withBaseURL(state, url)})
	}
	gen(header, Header{
		Root: Dir{
			Name: strings.TrimPrefix(strings.TrimSuffix(state.BaseURL.Path, "/"), "/"),
			URL:  state.BaseURL.String(),
		},
		Parts:      p,
		FullPath:   path.Join(state.BaseURL.Path+rel) + "/",
		Stylesheet: template.CSS(style),
	}, outBuff)
}

// populate the back line
func generateBackLine(state *ProgramState, parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	if len(parts) != 0 {
		gen(line, Line{
			IsDir: true,
			Name:  "..",
			URL:   withBaseURL(state, path.Join(rel, "..")),
			Size:  humanize.Bytes(0),
		}, outBuff)
	}
}

func generateDirectories(dirs []Directory, state *ProgramState, parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	dirName := path.Join(state.SrcDir, rel)
	outDir := path.Join(state.DestDir, rel)
	for _, dirEntry := range dirs {
		dirPath := path.Join(dirName, dirEntry.Name)
		// Avoid recursive infinite loop
		if dirPath == outDir {
			continue
		}

		data := Line{
			IsDir: true,
			Name:  dirEntry.Name,
			URL:   withBaseURL(state, path.Join(rel, dirEntry.Name)),
			Size:  humanize.Bytes(uint64(dirEntry.Size)),
			Date:  dirEntry.ModTime,
		}

		// FIX: fix empty flag here, i shouldnt generate if dir is empty!
		writeHTML(state, &dirEntry, append(parts, dirEntry.Name))
		gen(line, data, outBuff)
	}
}

func generateFiles(files []File, state *ProgramState, parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	dirName := path.Join(state.SrcDir, rel)
	outDir := path.Join(state.DestDir, rel)
	for _, fileEntry := range files {

		filePath := path.Join(dirName, fileEntry.Name)
		data := Line{
			IsDir: false,
			Name:  fileEntry.Name,
			URL:   withBaseURL(state, path.Join(rel, fileEntry.Name)),
			Size:  humanize.Bytes(uint64(fileEntry.Size)),
			Date:  fileEntry.ModTime,
		}
		if strings.HasSuffix(filePath, linkSuffix) {
			data.Name = data.Name[:len(data.Name)-len(linkSuffix)] // get name without extension
			data.Size = humanize.Bytes(0)

			raw, err := ioutil.ReadFile(filePath)
			if err != nil {
				log.Fatalf("Could not read link file: %s\n%s\n", filePath, err)
			}
			rawStr := string(raw)
			u, err := url.Parse(strings.TrimSpace(rawStr))
			if err != nil {
				log.Fatalf("Could not parse URL in file: %s\nThe value is: %s\n%s\n", filePath, raw, err)
			}

			data.URL = u.String()
			gen(line, data, outBuff)
			continue
		}
		gen(line, data, outBuff)
		// Copy all files over to the web root
		copy(filePath, path.Join(outDir, fileEntry.Name))
	}
}

// END REGION GENERATE

func writeHTML(state *ProgramState, directory *Directory, parts []string) {
	directory.Files = filterFiles(state, directory.Files)
	directory.Directories = filterDirs(state, directory.Directories)

	rel := path.Join(parts...)
	srcDirName := path.Join(state.SrcDir, rel)
	outDir := path.Join(state.DestDir, rel)
	log.Printf("Copying from %s\n", srcDirName)
	log.Printf("To directory %s\n", outDir)
	// FIXME
	if len(directory.Directories)+len(directory.Files) == 0 {
		return // state.IsEmpty
	}

	if state.EnableSort {
		// TODO: fix these types!!!! i cant run sort on directory and files!
		// sortAlphabetically(directory.Files)
	}

	// CHECK IF OUTPUT DIRECTORY HAS GOOD PERMS
	err := os.Mkdir(outDir, os.ModePerm)
	if err != nil {
		log.Fatalf("Could not create output *sub*directory: %s\n%s\n", outDir, err)
	}

	// CREATE HTMLFILE
	htmlPath := path.Join(outDir, "index.html")
	html, err := os.OpenFile(htmlPath, os.O_RDWR|os.O_CREATE, 0666)
	if err != nil {
		log.Fatalf("Could not create output index.html: %s\n%s\n", htmlPath, err)
	}

	out := new(bytes.Buffer)
	generateHeader(state, parts, out)
	generateBackLine(state, parts, out)
	generateDirectories(directory.Directories, state, parts, out)
	generateFiles(directory.Files, state, parts, out)
	gen(footer, Footer{Date: time.Now()}, out)

	err = state.Minifier.Minify("text/html", html, out)
	if err != nil {
		log.Fatalf("Could not write to index.html: %s\n%s\n", htmlPath, err)
	}
	err = html.Close()
	if err != nil {
		log.Fatalf("Could not write to close index.html: %s\n%s\n", htmlPath, err)
	}
	log.Printf("Generated data for directory: %s\n", srcDirName)
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
	log.Println("\tInclude:\t", state.IncludeRegExStr)
	log.Println("\tExclude:\t", state.ExcludeRegExStr)
	log.Println("\tRecursive:\t", state.IsRecursive)
	log.Println("\tEmpty:\t\t", state.IsEmpty)
	log.Println("\tConvert links:\t", state.ConvertLink)
	log.Println("\tSource:\t\t", state.SrcDir)
	log.Println("\tDestination:\t", state.DestDir)
	log.Println("\tBase URL:\t", state.URL)
	log.Println("\tStyle:\t\t", state.StyleTemplate)
	log.Println("\tFooter:\t\t", state.FooterTemplate)
	log.Println("\tHeader:\t\t", state.HeaderTemplate)
	log.Println("\tline:\t\t", state.LineTemplate)
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
	state.IncludeRegExStr = *flag.String("i", ".*", "A regex pattern to include files into the listing")
	state.ExcludeRegExStr = *flag.String("e", "\\.git(hub)?", "A regex pattern to exclude files from the listing")
	state.IsRecursive = *flag.Bool("r", true, "Recursively scan the file tree")
	state.IsEmpty = *flag.Bool("empty", false, "Whether to list empty directories")
	state.EnableSort = *flag.Bool("sort", true, "Sort files A-z and by type")
	state.URL = *flag.String("b", "http://localhost", "The base URL")
	state.ConvertLink = *flag.Bool("l", false, "Convert .link files to anchor tags")
	state.StyleTemplate = *flag.String("style", "", "Use a custom stylesheet file")
	state.FooterTemplate = *flag.String("footer", "", "Use a custom footer template")
	state.HeaderTemplate = *flag.String("header", "", "Use a custom header template")
	state.LineTemplate = *flag.String("line", "", "Use a custom line template")
	state.SrcDir = defaultSrc
	state.DestDir = defaultDest
	flag.Parse()

	args := flag.Args()
	if len(args) > 2 {
		log.Fatal("Invalid number of arguments, expected two at max (source and dest)")
	}
	if len(args) == 1 {
		state.DestDir = args[0]
	} else if len(args) == 2 {
		state.SrcDir = args[0]
		state.DestDir = args[1]
	}

	// NOTA: in seguito queste funzioni di logging si possono mettere in if con una flag per verbose
	logState(state)
	state.SrcDir = getAbsolutePath(state.SrcDir)
	state.DestDir = getAbsolutePath(state.DestDir)

	var err error
	state.IncludeRegEx, err = regexp.Compile(state.IncludeRegExStr)
	if err != nil {
		log.Fatal("Invalid regexp for include matching", err)
	}
	state.ExcludeRegEx, err = regexp.Compile(state.ExcludeRegExStr)
	if err != nil {
		log.Fatal("Invalid regexp for exclude matching", err)
	}

	state.BaseURL, err = url.Parse(state.URL)
	if err != nil {
		log.Fatalf("Could not parse base URL: %s\n%s\n", state.URL, err)
	}

	state.Minifier = minify.New()
	state.Minifier.AddFunc("text/css", css.Minify)
	state.Minifier.AddFunc("text/html", html.Minify)
	state.Minifier.AddFunc("application/javascript", js.Minify)

	// TODO: use the registry design pattern to generalize the template loading, parsing and execution
	// This section should not belong to initProgram because it doesnt modify things on ProgramState,
	// just needs access
	loadTemplate("header", state.HeaderTemplate, &rawHeader, &header)
	loadTemplate("line", state.LineTemplate, &rawLine, &line)
	loadTemplate("footer", state.FooterTemplate, &rawFooter, &footer)

	if state.StyleTemplate != "" {
		var content []byte
		if content, err = ioutil.ReadFile(state.StyleTemplate); err != nil {
			log.Fatalf("Could not read stylesheet file %s:\n%s\n", state.StyleTemplate, err)
		}
		style = string(content)
	}
}

func prepareDirectories(source, dest string) {
	// TODO: add fix for the case where source = "../../" as discussed

	// check inputDir is readable
	var err error
	_, err = os.OpenFile(source, os.O_RDONLY, 0666)
	if err != nil && os.IsPermission(err) {
		log.Fatalf("Could not read input directory: %s\n%s\n", source, err)
	}

	// check if outputDir is writable
	_, err = os.OpenFile(dest, os.O_WRONLY, 0666)
	if err != nil && os.IsPermission(err) {
		log.Fatalf("Could not write in output directory: %s\n%s\n", dest, err)
	}

	clearDirectory(dest)
}

func main() {
	var state ProgramState
	var srcStructure Directory
	initProgram(&state)
	prepareDirectories(state.SrcDir, state.DestDir)
	err := GetDirectoryStructure(state.SrcDir, state.IsRecursive, &srcStructure)
	if err != nil {
		log.Fatalf("Error when creating the directory structure:\n%s\n", err)
	}

	writeHTML(&state, &srcStructure, []string{})
}
