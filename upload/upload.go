package upload

import (
	"encoding/base64"
	"github.com/google/uuid"
	"github.com/gorilla/mux"
	"guptaspi/info"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Upload struct {
	FilePath       string
	FileSize       uint64
	Offset         uint64
	ExpirationDate time.Time
}

var uploadMap = map[uuid.UUID]*Upload{}
var lock = sync.RWMutex{}

func AddUploadRouter(r *mux.Router) {
	r.HandleFunc("/upload/{volume}", startUpload).Methods("POST")
	r.HandleFunc("/upload/{id}", headUpload).Methods("HEAD")
}

func startUpload(w http.ResponseWriter, r *http.Request) {
	volume := mux.Vars(r)["volume"]

	// Process overwrite query
	overwriteValue := r.FormValue("overwrite")
	var overwrite bool
	if overwriteValue != "" {
		var err error
		overwrite, err = strconv.ParseBool(overwriteValue)

		if err != nil {
			log.Printf("Overwrite query param error, %v", err)
			w.WriteHeader(400)
			return
		}
	} else {
		overwrite = false
	}

	// Tus version header
	if r.Header.Get("Tus-Resumable") != "1.0.0" {
		w.WriteHeader(405)
		return
	}

	metadata := processMetadata(r.Header.Get("Upload-Metadata"))

	// Get filepath
	b64FilePath, ok := metadata["filename"]
	if !ok {
		w.WriteHeader(400)
		return
	}
	filePathBytes, err := base64.StdEncoding.DecodeString(b64FilePath)
	if err != nil {
		log.Printf("B64 Decode error: %v", err)
		w.WriteHeader(400)
		return
	}
	filePath := string(filePathBytes)

	drive := info.GetDrive(volume)

	if drive == nil {
		w.WriteHeader(404)
		return
	}

	filePath = filepath.Join(drive.Path, filePath)

	var uploadLength uint64

	// Upload length and deferral
	if deferLength := r.Header.Get("Upload-Defer-Length"); deferLength != "" {
		if deferLength != "1" {
			w.WriteHeader(400)
			return
		}
		if r.Header.Get("Upload-Length") != "" {
			w.WriteHeader(400)
			return
		}
		uploadLength = 0
	} else {
		uploadLength, err = strconv.ParseUint(r.Header.Get("Upload-Length"), 10, 64)
		if err != nil {
			w.WriteHeader(400)
			return
		}
		if uploadLength <= 0 {
			w.WriteHeader(400)
			return
		}
	}

	// File creation
	var file *os.File
	if !overwrite {
		file, err = os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0777)
	} else {
		file, err = os.OpenFile(filePath, os.O_WRONLY|os.O_CREATE, 0777)
	}
	if err != nil {
		log.Printf("File Creation error: %v", err)
		if err.Error() == "open "+filePath+": The file exists." {
			w.WriteHeader(409)
			return
		}
		w.WriteHeader(500)
		return
	}

	_, err = file.Seek(int64(uploadLength-1), 0)
	if err != nil {
		log.Printf("File Seek error: %v", err)
		w.WriteHeader(500)
		if err := file.Close(); err != nil {
			log.Printf("Failed to close file: %v", err)
		}
		return
	}

	_, err = file.Write([]byte{0})
	if err != nil {
		log.Printf("File Write error: %v", err)
		w.WriteHeader(500)
		if err := file.Close(); err != nil {
			log.Printf("Failed to close file: %v", err)
		}
		return
	}

	if err := file.Close(); err != nil {
		log.Printf("Failed to close file: %v", err)
	}

	// UUID generation
	id, err := uuid.NewRandom()
	if err != nil {
		log.Printf("UUID creation error: %v", err)
		w.WriteHeader(500)
		return
	}

	upload := Upload{
		FilePath:       filePath,
		FileSize:       uploadLength,
		Offset:         0,
		ExpirationDate: time.Now().UTC().Add(time.Hour * time.Duration(1)),
	}

	lock.Lock()
	uploadMap[id] = &upload
	lock.Unlock()

	w.Header().Add("Tus-Removable", "1.0.0")
	w.Header().Add("Upload-Expires", upload.ExpirationDate.Format(time.RFC3339))
	w.Header().Add("Location", "http://localhost:5000/upload/"+id.String())
	w.WriteHeader(201)
}

func headUpload(w http.ResponseWriter, r *http.Request) {
	idString := mux.Vars(r)["id"]

	id, err := uuid.Parse(idString)
	if err != nil {
		log.Printf("Error parsing uuid: %v", err)
		w.WriteHeader(400)
		return
	}

	lock.RLock()
	upload, ok := uploadMap[id]
	lock.RUnlock()

	if !ok {
		w.WriteHeader(404)
		return
	}

	w.Header().Add("Upload-Offset", strconv.FormatUint(upload.Offset, 10))
	w.Header().Add("Upload-Expires", upload.ExpirationDate.Format(time.RFC3339))
	w.Header().Add("Tus-Resumable", "1.0.0")
	w.Header().Add("Cache-Control", "no-store")

	if upload.FileSize > 0 {
		w.Header().Add("Upload-Length", strconv.FormatUint(upload.FileSize, 10))
	} else {
		w.Header().Add("Upload-Defer-Length", "1")
	}

	w.WriteHeader(200)
}

func processMetadata(metadataText string) map[string]string {
	pairs := strings.Split(metadataText, ",")
	metadata := make(map[string]string, len(pairs))

	for _, pair := range pairs {
		keyValue := strings.Split(pair, " ")
		if len(keyValue) == 2 {
			metadata[keyValue[0]] = keyValue[1]
		} else {
			metadata[keyValue[0]] = ""
		}
	}
	return metadata
}
