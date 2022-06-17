package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
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
	"github.com/gabriel-vasile/mimetype"
	"github.com/tdewolff/minify/v2"
	"github.com/tdewolff/minify/v2/css"
	"github.com/tdewolff/minify/v2/html"
	"github.com/tdewolff/minify/v2/js"
)

// Describes the state of every main variable of the program
var (
	//go:embed "header.gohtml"
	headerTemplate 		 string
	//go:embed "line.gohtml"
	lineTemplate 		 string
	//go:embed "footer.gohtml"
	footerTemplate		 string
	//go:embed "style.css"
	style                string
	header, footer, line *template.Template
	minifier             *minify.M

	styleTemplatePath	string
	headerTemplatePath	string
	lineTemplatePath	string
	footerTemplatePath	string

	workDir string
	srcDir  string
	destDir string

	isRecursive     bool
	isEmpty         bool
	enableSort      bool
	convertLink     bool
	includeRegEx    *regexp.Regexp
	excludeRegEx    *regexp.Regexp
	includeRegExStr string
	excludeRegExStr string
	baseURL         *url.URL
	rawURL			string
)

const (
	linkSuffix  = ".link"
	defaultSrc  = "./"
	defaultDest = "site"
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

type Named interface {
	GetName() string
}

type Directory struct {
	Name        string      `json:"name"`
	Path        string      `json:"path"`
	SourcePath 	string  	`json:"-"`
	Directories []Directory `json:"directories"`
	Files       []File      `json:"files"`
	Size        int64       `json:"size"`
	ModTime     time.Time   `json:"time"`
}

type FuzzyFile struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	SourcePath string   `json:"-"`
	URL        *url.URL `json:"url"`
	Mime       string   `json:"mime"`
}

type File struct {
	FuzzyFile
	Size    int64     `json:"size"`
	ModTime time.Time `json:"time"`
}

func (d Directory) GetName() string { return d.Name }
func (f File) GetName() string      { return f.FuzzyFile.Name }

// joins the baseURL with the given relative path in a new URL instance
func withBaseURL(rel string) (url *url.URL) {
	url, _ = url.Parse(baseURL.String()) // its clear that there can't be error here :)
	url.Path = path.Join(baseURL.Path, rel)
	return
}

func newFile(info os.FileInfo, dir string) (f File, err error) {
	if info.IsDir() {
		return File{}, errors.New("newFile has been called with a os.FileInfo of type Directory")
	}

	var (
		rel  string
		url  *url.URL
		mime *mimetype.MIME
	)
	abs := path.Join(dir, info.Name())
	if rel, err = filepath.Rel(srcDir, abs); err != nil {
		return
	}
	url = withBaseURL(rel)
	if mime, err = mimetype.DetectFile(abs); err != nil {
		return
	}

	return File{
		FuzzyFile: FuzzyFile{
			Name: info.Name(),
			Path: rel,
			SourcePath: dir,
			URL:  url,
			Mime: mime.String(),
		},
		Size:    info.Size(),
		ModTime: info.ModTime(),
	}, nil
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

func (d Directory) isEmpty() bool {
	return len(d.Directories) == 0 && len(d.Files) == 0
}

func sortAlpha[T Named](infos []T) {
	sort.Slice(infos, func(i, j int) bool {
		return infos[i].GetName() < infos[j].GetName()
	})
}

func walk(base string) (dir Directory, err error) {
	var (
		infos		[]fs.FileInfo
		sourceInfo 	fs.FileInfo
		subdir		Directory
		file	  	File
		rel    		string
	)
	if infos, err = ioutil.ReadDir(base); err != nil {
		return dir, fmt.Errorf("Could not read directory %s:\n%s", base, err)
	}

	if sourceInfo, err = os.Stat(base); err != nil {
		return dir, fmt.Errorf("Could not stat directory %s:\n%s", base, err)
	}

	if rel, err = filepath.Rel(srcDir, base); err != nil {
		return dir, err
	}

	dir = Directory{
		Name:    	sourceInfo.Name(),
		SourcePath: base,
		Path:    	rel,
		Size:    	sourceInfo.Size(),
		ModTime: 	sourceInfo.ModTime(),
	}

	for _, info := range infos {
		if info.IsDir() && isRecursive && includeDir(info) {
			if subdir, err = walk(path.Join(base, info.Name())); err != nil {
				return dir, err
			}
			if !subdir.isEmpty() || isEmpty { // include emptydir if isEmptyflag is setted
				dir.Directories = append(dir.Directories, subdir)
			}
		} else if !info.IsDir() && includeFile(info) {
			if file, err = newFile(info, base); err != nil {
				return dir, fmt.Errorf("Error while generating the File structure:\n%s", err)
			}
			dir.Files = append(dir.Files, file)
		}
	}
	sortAlpha(dir.Files)
	sortAlpha(dir.Directories)
	return
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

func includeDir(info fs.FileInfo) bool {
	return !excludeRegEx.MatchString(info.Name())
}

func includeFile(info fs.FileInfo) bool {
	return includeRegEx.MatchString(info.Name()) && !excludeRegEx.MatchString(info.Name())
}

// REGION GENERATE
// TODO: these functions should be later generalized with interface and so on...
// the function parameters are temporary, i have to find a way to reduce it...

// Generate the header and the double dots back anchor when appropriate
func generateHeader(parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	p, url := []Dir{}, ""
	for _, part := range parts {
		url = path.Join(url, part)
		p = append(p, Dir{Name: part, URL: withBaseURL(url).Path})
	}
	gen(header, Header{
		Root: Dir{
			Name: strings.TrimPrefix(strings.TrimSuffix(baseURL.Path, "/"), "/"),
			URL:  baseURL.String(),
		},
		Parts:      p,
		FullPath:   path.Join(baseURL.Path+rel) + "/",
		Stylesheet: template.CSS(style),
	}, outBuff)
}

// populate the back line
func generateBackLine(parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	if len(parts) != 0 {
		gen(line, Line{
			IsDir: true,
			Name:  "..",
			URL:   withBaseURL(path.Join(rel, "..")).Path,
			Size:  humanize.Bytes(0),
		}, outBuff)
	}
}

func generateDirectories(dirs []Directory, parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	dirName := path.Join(srcDir, rel)
	outDir := path.Join(destDir, rel)
	for _, dirEntry := range dirs {
		dirPath := path.Join(dirName, dirEntry.Name)
		// Avoid recursive infinite loop
		if dirPath == outDir {
			continue
		}

		data := Line{
			IsDir: true,
			Name:  dirEntry.Name,
			URL:   withBaseURL(path.Join(rel, dirEntry.Name)).Path,
			Size:  humanize.Bytes(uint64(dirEntry.Size)),
			Date:  dirEntry.ModTime,
		}

		writeHTML(&dirEntry, append(parts, dirEntry.Name))
		gen(line, data, outBuff)
	}
}

func generateFiles(files []File, parts []string, outBuff *bytes.Buffer) {
	rel := path.Join(parts...)
	dirName := path.Join(srcDir, rel)
	outDir := path.Join(destDir, rel)
	for _, fileEntry := range files {

		// DEBUG REMOVE ME AFTER FIX
		fmt.Printf("file probably generated: %s \n", fileEntry.Name)
		filePath := path.Join(dirName, fileEntry.Name)
		data := Line{
			IsDir: false,
			Name:  fileEntry.Name,
			URL:   withBaseURL(path.Join(rel, fileEntry.Name)).Path,
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
		} else {
			// Copy all files over to the web root
			copy(filePath, path.Join(outDir, fileEntry.Name))
		}
		gen(line, data, outBuff)
	}
}

// END REGION GENERATE

func writeHTML(directory *Directory, parts []string) {
	rel := path.Join(parts...)
	srcDirName := path.Join(srcDir, rel)
	outDir := path.Join(destDir, rel)

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
	generateHeader(parts, out)
	generateBackLine(parts, out)
	generateDirectories(directory.Directories, parts, out)
	generateFiles(directory.Files, parts, out)
	gen(footer, Footer{Date: time.Now()}, out)

	err = minifier.Minify("text/html", html, out)
	if err != nil {
		log.Fatalf("Could not write to index.html: %s\n%s\n", htmlPath, err)
	}
	err = html.Close()
	if err != nil {
		log.Fatalf("Could not write to close index.html: %s\n%s\n", htmlPath, err)
	}
	log.Printf("Generated data for directory: %s\n", srcDirName)
}

func readIfNotEmpty(path string, dest *string) (err error) {
	var content []byte
	if path != "" {
		content, err = ioutil.ReadFile(path)
		if err != nil {
			return fmt.Errorf("Could not read file: %s\n%s", path, err)
		}
	}
	*dest = string(content)
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

// TODO: debug: remove me after fix!
func loadTemplateOld(name string, path string, def *string, dest **template.Template) {
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


func logState() {
	log.Println("Running with parameters:")
	log.Println("\tInclude:\t", includeRegExStr)
	log.Println("\tExclude:\t", excludeRegExStr)
	log.Println("\tRecursive:\t", isRecursive)
	log.Println("\tEmpty:\t\t", isEmpty)
	log.Println("\tConvert links:\t", convertLink)
	log.Println("\tSource:\t\t", srcDir)
	log.Println("\tDestination:\t", destDir)
	log.Println("\tBase URL:\t", rawURL)
	log.Println("\tStyle:\t\t", styleTemplatePath)
	log.Println("\tFooter:\t\t", footerTemplatePath)
	log.Println("\tHeader:\t\t", headerTemplatePath)
	log.Println("\tline:\t\t", lineTemplatePath)
}

func getAbsPath(rel string) string {
	if filepath.IsAbs(rel) {
		return rel
	}

	return path.Join(workDir, rel)
}

func sanitizeDirectories() (err error) {
	if strings.HasPrefix(srcDir, destDir) {
		return errors.New("The output directory cannot be a parent of the input directory")
	}

	if _, err = os.OpenFile(srcDir, os.O_RDONLY, 0666); err != nil && os.IsPermission(err) {
		return fmt.Errorf("Cannot open source directory for reading: %s\n%s", srcDir, err)
	}

	if err := isDir(srcDir); err != nil {
		return err
	}

	// check if outputDir is writable
	if _, err = os.OpenFile(destDir, os.O_WRONLY, 0666); err != nil && os.IsPermission(err) {
		return fmt.Errorf("Cannot open output directory for writing: %s\n%s", destDir, err)
	}

	if err = os.RemoveAll(destDir); err != nil {
		return fmt.Errorf("Cannot clear output directory: %s\n%s", destDir, err)
	}
	return nil
}

func main() {
	// REGION INITGLOBALS
	var err error
	includeRegExStr = *flag.String("i", ".*", "A regex pattern to include files into the listing")
	excludeRegExStr = *flag.String("e", "\\.git(hub)?", "A regex pattern to exclude files from the listing")
	isRecursive = *flag.Bool("r", true, "Recursively scan the file tree")
	isEmpty = *flag.Bool("empty", false, "Whether to list empty directories")
	enableSort = *flag.Bool("sort", true, "Sort files A-z and by type")
	rawURL = *flag.String("b", "http://localhost", "The base URL")
	convertLink = *flag.Bool("l", false, "Convert .link files to anchor tags")
	styleTemplatePath = *flag.String("style", "", "Use a custom stylesheet file")
	headerTemplatePath = *flag.String("header", "", "Use a custom header template")
	lineTemplatePath = *flag.String("line", "", "Use a custom line template")
	footerTemplatePath = *flag.String("footer", "", "Use a custom footer template")
	srcDir = defaultSrc
	destDir = defaultDest
	flag.Parse()

	args := flag.Args()
	if len(args) > 2 {
		fmt.Printf("Usage: %s [dest] or [src] [dest]\n", os.Args[0])
		os.Exit(1)
	}
	if len(args) == 1 {
		destDir = args[0]
	} else if len(args) == 2 {
		srcDir = args[0]
		destDir = args[1]
	}

	if workDir, err = os.Getwd(); err != nil {
		log.Fatal("Could not get working directory", err)
	}

	// NOTA: in seguito queste funzioni di logging si possono mettere in if con una flag per verbose
	logState()
	srcDir = getAbsPath(srcDir)
	destDir = getAbsPath(destDir)
	if err = sanitizeDirectories(); err != nil {
		log.Fatal("Error while checking src and dest paths", err)
	}

	if includeRegEx, err = regexp.Compile(includeRegExStr); err != nil {
		log.Fatal("Invalid regexp for include matching", err)
	}
	if excludeRegEx, err = regexp.Compile(excludeRegExStr); err != nil {
		log.Fatal("Invalid regexp for exclude matching", err)
	}

	if baseURL, err = url.Parse(rawURL); err != nil {
		log.Fatal("Could not parse base URL", err)
	}

	minifier = minify.New()
	minifier.AddFunc("text/css", css.Minify)
	minifier.AddFunc("text/html", html.Minify)
	minifier.AddFunc("application/javascript", js.Minify)

	loadTemplateOld("header", headerTemplatePath, &headerTemplate, &header)

	// if header, err = loadTemplate("header", headerTemplatePath, &headerTemplate); err != nil {
	// 	log.Fatal("Could not parse header template", err)
	// }
	if line, err = loadTemplate("line", lineTemplatePath, &lineTemplate); err != nil {
		log.Fatal("Could not parse line template", err)
	}
	if footer, err = loadTemplate("footer", footerTemplatePath, &footerTemplate); err != nil {
		log.Fatal("Could not parse footer template", err)
	}
	if err = readIfNotEmpty(styleTemplatePath, &style); err != nil {
		log.Fatal("Could not read stylesheet file", err)
	}
	// ENDREGION INITGLOBALS

	dir, err := walk(srcDir)
	if err != nil {
		log.Fatalf("Error when creating the directory structure:\n%s\n", err)
	}
	// DEBUG REMOVE ME AFTER FIX
	fmt.Printf("%+v\n", dir)
	writeHTML(&dir, []string{})
}
