package main

import (
	"crypto/md5"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/russross/smugmug"
)

var (
	apiKey   string
	email    string
	password string
	dir      string
	dry      bool
	del      bool

	fileCount  int
	totalBytes int
)

func main() {
	start := time.Now()

	// parse config
	configString(&apiKey, "apikey", "", "SmugMug API key")
	configString(&email, "email", "", "Email address")
	configString(&password, "password", "", "Password")
	configString(&dir, "dir", "", "Target directory")
	flag.BoolVar(&dry, "dry", false, "Dry run (no changes)")
	flag.BoolVar(&del, "delete", true, "Delete local files")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("Unknown command-line options: %s", strings.Join(flag.Args(), " "))
	}
	if apiKey == "" || email == "" || password == "" {
		log.Fatalf("apikey, email, and password are all required")
	}
	if dir == "" {
		dir = "."
	}
	d, err := filepath.Abs(dir)
	if err != nil {
		log.Fatalf("Unable to find absolute path for %s: %v", dir, err)
	}
	dir = d

	// login
	c, err := smugmug.Login(email, password, apiKey)
	if err != nil {
		log.Fatalf("Login error: %v", err)
	}
	log.Printf("Logged in %s, NickName is %s", email, c.NickName)

	// scan the local directory: map path to md5sum
	log.Printf("Scanning local file system, this may take some time")
	localFiles := make(map[string]string)
	if err := filepath.Walk(dir, filepath.WalkFunc(func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		suffix := path
		if strings.HasPrefix(path, dir+"/") {
			suffix = path[len(dir)+1:]
		}

		if info.IsDir() {
			localFiles[suffix] = "directory"
			return nil
		}

		// get an MD5 hash
		h := md5.New()
		f, err := os.Open(path)
		if err != nil {
			log.Printf("error opening %s: %v", path, err)
			return err
		}
		defer f.Close()
		if _, err = io.Copy(h, f); err != nil {
			log.Printf("error reading %s: %v", path, err)
			return err
		}
		sum := h.Sum(nil)
		s := hex.EncodeToString(sum)
		localFiles[suffix] = s
		return nil
	})); err != nil {
		log.Fatalf("error walking local file system: %v", err)
	}

	// get full list of albums
	albums, err := c.Albums(c.NickName)
	if err != nil {
		log.Fatalf("Albums error: %v", err)
	}
	log.Printf("Found %d albums", len(albums))

	// process each album
	for _, ainfo := range albums {
		log.Printf("Processing album %s in category %s [%s]", ainfo.Title, ainfo.Category.Name, ainfo.URL)

		// get full list of images from this album
		images, err := c.Images(ainfo)
		if err != nil {
			log.Fatalf("Images error: %v", err)
		}

		// process each image
		for _, img := range images {
			if err := sync(ainfo, img, localFiles, dir); err != nil {
				log.Fatalf("Error processing image %s from album %s in category %s: %v",
					img.FileName, ainfo.Title, ainfo.Category.Name, err)
			}
		}
	}

	if err = cleanup(localFiles, dir); err != nil {
		log.Fatalf("Error cleaning up: %v", err)
	}

	if totalBytes > 1024*1024 {
		log.Printf("Downloaded %d files (%.1fm) in %v", fileCount, float64(totalBytes)/(1024*1024), time.Since(start))
	} else if totalBytes > 1024 {
		log.Printf("Downloaded %d files (%.1fk) in %v", fileCount, float64(totalBytes)/1024, time.Since(start))
	} else {
		log.Printf("Downloaded %d files (%d bytes) in %v", fileCount, totalBytes, time.Since(start))
	}
}

func sync(album *smugmug.AlbumInfo, image *smugmug.ImageInfo, localFiles map[string]string, dir string) error {
	path := album.Category.Name
	if album.SubCategory != nil {
		path = filepath.Join(path, album.SubCategory.Name)
	}
	path = filepath.Join(path, album.Title)
	if image.FileName != "" {
		path = filepath.Join(path, image.FileName)
	} else {
		return fmt.Errorf("image with no filename: ID=%d Key=%s Album=%v", image.ID, image.Key, image.Album)
	}

	if localFiles[path] == image.MD5Sum {
		log.Printf("    skipping unchanged file %s", path)

		// mark this local file as existing on the server
		delete(localFiles, path)
		delete(localFiles, filepath.Dir(path))

		return nil
	}

	// file is new/changed, so download it
	fullpath := filepath.Join(dir, path)

	changed := "(new file)"
	if localFiles[path] != "" {
		changed = "(file changed)"
	}

	// mark this local file as existing on the server
	delete(localFiles, path)
	delete(localFiles, filepath.Dir(path))

	if dry {
		log.Printf("    %s: dry run, no downloading %s", path, changed)
		totalBytes += image.Size
		fileCount++
		return nil
	}

	resp, err := http.Get(image.OriginalURL)
	if err != nil {
		return fmt.Errorf("error downloading %s: %v", image.OriginalURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code downloading %s: %d", image.OriginalURL, resp.StatusCode)
	}

	// create the directory if necessary
	if err = os.MkdirAll(filepath.Dir(fullpath), 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %v", filepath.Dir(fullpath), err)
	}
	fp, err := os.Create(fullpath)
	if err != nil {
		return fmt.Errorf("failed to open %s for writing: %v", fullpath, err)
	}
	defer fp.Close()
	size, err := io.Copy(fp, resp.Body)
	if err != nil {
		return fmt.Errorf("error saving file %s: %v", fullpath, err)
	}
	if int(size) != image.Size {
		return fmt.Errorf("downloaded %d bytes from %s, expected %d", size, image.OriginalURL, image.Size)
	}
	if size > 1024*1024 {
		log.Printf("    %s: downloaded %.1fm %s", path, float64(size)/(1024*1024), changed)
	} else if size > 1024 {
		log.Printf("    %s: downloaded %.1fk %s", path, float64(size)/1024, changed)
	} else {
		log.Printf("    %s: downloaded %d bytes %s", path, size, changed)
	}
	totalBytes += int(size)
	fileCount++

	return nil
}

func cleanup(localFiles map[string]string, dir string) error {
	if !del {
		return nil
	}

	// delete local file not found on server
	for k, v := range localFiles {
		if v == "directory" {
			continue
		}
		if dry {
			log.Printf("dry run, not removing file %s", k)
		} else {
			fullpath := filepath.Join(dir, k)
			if err := os.Remove(fullpath); err != nil {
				return fmt.Errorf("error removing file %s: %v", fullpath, err)
			}
		}
	}

	// delete directories found but not used
	for k, v := range localFiles {
		if v != "directory" {
			continue
		}
		if dry {
			log.Printf("dry run, not removing directory %s", k)
		} else {
			fullpath := filepath.Join(dir, k)
			if err := os.Remove(fullpath); err != nil {
				return fmt.Errorf("error removing directory %s: %v", fullpath, err)
			}
		}
	}

	log.Printf("removed %d files and directories", len(localFiles))
	return nil
}

// configString sets a config variable with a string value
// in ascending priority:
// 1. Default value passed in
// 2. Environment variable value (name in upper case)
// 3. Command-line argument (parameters mimic flag.StringVar)
func configString(p *string, name, value, usage string) {
	if s := os.Getenv(strings.ToUpper(name)); s != "" {
		// set it to environment value if available
		*p = s
	} else {
		// fall back to default
		*p = value
	}

	// pass it on to flag
	flag.StringVar(p, name, *p, usage)
}
