package main

import (
	"flag"
	"fmt"
	"http"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"sync"
	"time"
	"xml"
)

var flagBase *string = flag.String("base", "http://www.picpix.com/kelly",
	"e.g. http://www.picpix.com/username (no trailing slash)")

var flagDest *string = flag.String("dest", "/home/bradfitz/backup/picpix/kelly", "Destination backup root")

var flagSloppy *bool = flag.Bool("sloppy", false, "Continue on errors")

var galleryMutex sync.Mutex
var galleryMap map[string]*Gallery = make(map[string]*Gallery)

var picMutex sync.Mutex
var picMap map[string]*Pic = make(map[string]*Pic)

// Only allow 10 network fetches at a time.
var fetchGate chan bool = make(chan bool, 10)

var localOpGate chan bool = make(chan bool, 100)

var opsMutex sync.Mutex
var opsInFlight int

var errorMutex sync.Mutex
var errors []string = make([]string, 0)

var galleryPattern *regexp.Regexp = regexp.MustCompile(
	"/gallery/([0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z])")
var picPattern *regexp.Regexp = regexp.MustCompile(
	"/pic/([0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z][0-9a-z])")

func addError(msg string) {
	errorMutex.Lock()
	defer errorMutex.Unlock()
	errors = append(errors, msg)
	if *flagSloppy {
		log.Printf("ERROR: %s", msg)
	} else {
		log.Exitf("ERROR: %s", msg)
	}
}

type Operation interface {
	Done()
}

type NetworkOperation int
type LocalOperation int

func NewLocalOperation() Operation {
	opsMutex.Lock()
	opsInFlight++
	opsMutex.Unlock()
	localOpGate <- true  // buffer-limited, may/should block
	return LocalOperation(0)
}

func NewNetworkOperation() Operation {
	opsMutex.Lock()
	opsInFlight++
	opsMutex.Unlock()
        fetchGate <- true
	return NetworkOperation(0)
}

func (o LocalOperation) Done() {
	<-localOpGate
	opsMutex.Lock()
        defer opsMutex.Unlock()
	opsInFlight--
}

func (o NetworkOperation) Done() {
	<-fetchGate
	opsMutex.Lock()
        defer opsMutex.Unlock()
	opsInFlight--
}

func OperationsInFlight() int {
	opsMutex.Lock()
        defer opsMutex.Unlock()
	return opsInFlight
}

func fetchUrlToFile(url, filename string) bool {
	fi, statErr := os.Stat(filename)
	if statErr == nil && fi.Size > 0 {
		// TODO: re-fetch mode?
		return true
	}

	netop := NewNetworkOperation()
	defer netop.Done()

	res, _, err := http.Get(url)
	if err != nil {
		addError(fmt.Sprintf("Error fetching %s: %v", url, err))
		return false
	}
	defer res.Body.Close()

	fileBytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		addError(fmt.Sprintf("Error reading XML from %s: %v", url, err))
		return false
	}

	err = ioutil.WriteFile(filename, fileBytes, 0600)
 	if err != nil {
		addError(fmt.Sprintf("Error writing file %s: %v", filename, err))
		return false
	}
	return true
}

type Gallery struct {
	key  string
}

func (g *Gallery) XmlUrl() string {
	return fmt.Sprintf("%s/gallery/%s.xml", *flagBase, g.key)
}

func (g *Gallery) Fetch(op Operation) {
	defer op.Done()

	galXmlFilename := fmt.Sprintf("%s/gallery-%s.xml", *flagDest, g.key)
	if fetchUrlToFile(g.XmlUrl(), galXmlFilename) {
		go fetchPhotosInGallery(galXmlFilename, NewLocalOperation())
	}
}

type Pic struct {
	key  string
}

func (p *Pic) XmlUrl() string {
        return fmt.Sprintf("%s/pic/%s.xml", *flagBase, p.key)
}

func (p *Pic) BlobUrl() string {
        return fmt.Sprintf("%s/pic/%s", *flagBase, p.key)
}

func (p *Pic) XmlBackupFilename() string {
	return fmt.Sprintf("%s/pic-%s.xml", *flagDest, p.key)
}

func (p *Pic) Fetch(op Operation) {
        defer op.Done()
	if !fetchUrlToFile(p.XmlUrl(), p.XmlBackupFilename()) {
		return
	}
	
}

type DigestInfo struct {
	XMLName xml.Name "digest"
	Type  string "attr"
	Value string "chardata"
}

type MediaFile struct {
	XMLName xml.Name "file"
	Digest DigestInfo
	Mime string
	Width int
	Height int
	Bytes int
	Url string  // the raw URL
}

type MediaSetItem struct {
	XMLName xml.Name "mediaSetItem"
	Title string
	Description string
	InfoURL string  // the xml URL
	File MediaFile
}

type MediaSetItemsWrapper struct {
	XMLName xml.Name "mediaSetItems"
	MediaSetItem []MediaSetItem
}

type LinkedFromSet struct {
	XMLName xml.Name "linkedFrom"
	InfoURL []string   // xml gallery URLs of 'parent' galleries (not a DAG)
}

type LinkedToSet struct {
	XMLName xml.Name "linkedTo"
	InfoURL []string   // xml gallery URLs of 'children' galleries (not a DAG)
}

type MediaSet struct {
	XMLName xml.Name "mediaSet"
	MediaSetItems MediaSetItemsWrapper
	LinkedFrom  LinkedFromSet
	LinkedTo    LinkedToSet
}

func fetchPhotosInGallery(filename string, op Operation) {
	defer op.Done()

	f, err := os.Open(filename, os.O_RDONLY, 0)
	if err != nil {
		addError(fmt.Sprintf("Failed to open %s: %v", filename, err))
		return
	}
	defer f.Close()
	mediaSet := new(MediaSet)
	err = xml.Unmarshal(f, mediaSet)
	if err != nil {
                addError(fmt.Sprintf("Failed to unmarshal %s: %v", filename, err))
                return
        }

	// Learn about new galleries, potentially?
	for _, url := range mediaSet.LinkedFrom.InfoURL {
		noteGallery(url)
	}
	for _, url := range mediaSet.LinkedTo.InfoURL {
		noteGallery(url)
	}

	//log.Printf("Parse of %s is: %q", filename, mediaSet)
	for _, item := range mediaSet.MediaSetItems.MediaSetItem {
		//log.Printf("   pic: %s", item.InfoURL)
		notePhoto(item.InfoURL)
	}
}

func knownGalleries() int {
	galleryMutex.Lock()
	defer galleryMutex.Unlock()
	return len(galleryMap)
}

func findKey(keyOrUrl string, pattern *regexp.Regexp) string {
	if len(keyOrUrl) == 8 {
		return keyOrUrl
	}

	matches := pattern.FindStringSubmatch(keyOrUrl)
	if matches == nil {
		panic("Failed to parse: " + keyOrUrl)
	}
	if len(matches[1]) != 8 {
		panic("Expected match of 8 chars in " + keyOrUrl)
	}
	return matches[1]
}

func noteGallery(keyOrUrl string) {
	key := findKey(keyOrUrl, galleryPattern)
	galleryMutex.Lock()
	defer galleryMutex.Unlock()
	if _, known := galleryMap[key]; known {
		return
	}
	gallery := &Gallery{key}
	galleryMap[key] = gallery
	log.Printf("Gallery: %s", gallery.XmlUrl())
	go gallery.Fetch(NewLocalOperation())
}

func notePhoto(keyOrUrl string) {
	key := findKey(keyOrUrl, picPattern)
	picMutex.Lock()
	defer picMutex.Unlock()
	if _, known := picMap[key]; known {
		return
	}
	pic := &Pic{key}
	picMap[key] = pic
	log.Printf("Photo: %s", pic.XmlUrl())
	go pic.Fetch(NewLocalOperation())
}

func fetchGalleryPage(page int) {
	log.Printf("Fetching gallery page %d", page)
	res, finalUrl, err := http.Get(fmt.Sprintf("%s/?sort=alpha&page=%d",
		*flagBase, page))
	if err != nil {
		log.Exitf("Error fetching gallery page %d: %v", page, err)
	}
	log.Printf("Fetched page %d: %s", page, finalUrl)
	htmlBytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		log.Exitf("Error reading gallery page %d's HTML: %v", page, err)
	}
	res.Body.Close()

	html := string(htmlBytes)
	log.Printf("read %d bytes", len(html))

	matches := galleryPattern.FindAllStringSubmatch(html, -1)
	for _, match := range matches {
		noteGallery(match[1])
	}
}

func main() {
	flag.Parse()
	log.Printf("Starting.")

	page := 1
	for {
		countBefore := knownGalleries()
		fetchGalleryPage(page)
		countAfter := knownGalleries()
		log.Printf("Galleries known: %d", countAfter)
		if countAfter == countBefore {
			log.Printf("No new galleries, stopping.")
			break
		}
		page++
	}

	for {
		n := OperationsInFlight()
		if n == 0 {
			break
		}
		log.Printf("%d Operations in-flight.  Waiting.", n)
		time.Sleep(5 * 1e9)
	}
	log.Printf("Done.")
}