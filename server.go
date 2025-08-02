package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	uploadsDir = "/tmp/uploads"
	tempDir    = "/tmp/temp"
)

func init() {
	if err := os.MkdirAll(uploadsDir, 0755); err != nil {
		log.Fatalf("❌ Could not create uploads directory: %v", err)
	}
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		log.Fatalf("❌ Could not create temp directory: %v", err)
	}
	if err := InitDB(); err != nil {
		log.Fatalf("❌ Could not initialize database: %v", err)
	}
	log.Println("✅ Database connection successful.")
}

func calculateFileHash(filePath string) (string, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", hasher.Sum(nil)), nil
}

func processAndForwardFile(w http.ResponseWriter, filePath, filename, userhash, destination, timeVal string) {
	if destination == "litterbox" {
		log.Printf("🗑️ Litterbox file will not be cached. Proceeding with direct upload.")
		url, err := forwardToDestination(destination, filePath, filename, userhash, timeVal)
		if err != nil {
			log.Printf("❌ Upload error to litterbox: %v", err)
			http.Error(w, "Upload failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		log.Printf("🚀 Uploaded to %s: %s", destination, url)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": url})
		return
	}

	hash, err := calculateFileHash(filePath)
	if err != nil {
		log.Printf("❌ Failed to calculate file hash for %s: %v", filename, err)
		http.Error(w, "Failed to calculate file hash", http.StatusInternalServerError)
		return
	}

	record, err := GetURLsByHash(hash)
	if err != nil {
		log.Printf("❌ Database lookup failed for hash %s: %v", hash[:10], err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var cachedURL string
	if destination == "catbox" && record.CatboxURL.Valid {
		cachedURL = record.CatboxURL.String
	} else if destination == "pomf" && record.PomfURL.Valid {
		cachedURL = record.PomfURL.String
	}

	if cachedURL != "" {
		log.Printf("✅ Cache hit for hash %s on destination %s. Returning stored URL.", hash[:10], destination)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"url": cachedURL})
		return
	}

	log.Printf("🔍 Cache miss for hash %s on destination %s. Uploading...", hash[:10], destination)
	url, err := forwardToDestination(destination, filePath, filename, userhash, timeVal)
	if err != nil {
		log.Printf("❌ Upload error for hash %s to %s: %v", hash[:10], destination, err)
		http.Error(w, "Upload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := storeUrl(hash, destination, url); err != nil {
		log.Printf("⚠️ Failed to store hash %s for destination %s in database: %v", hash[:10], destination, err)
	}

	log.Printf("🚀 Uploaded to %s: %s", destination, url)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": url})
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		notFoundHandler(w, r)
		return
	}
	fmt.Fprintln(w, "fatbox is working.")
}

func notFoundHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"message":    fmt.Sprintf("Route %s:%s not found", r.Method, r.URL.Path),
		"error":      "Not Found",
		"statusCode": 404,
	})
}

func chunkHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		http.Error(w, "Invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	uploadId := r.FormValue("uploadId")
	index := r.FormValue("index")
	if uploadId == "" || index == "" {
		http.Error(w, "Missing uploadId or index", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("chunk")
	if err != nil {
		http.Error(w, "Missing chunk file", http.StatusBadRequest)
		return
	}
	defer file.Close()
	uploadPath := filepath.Join(uploadsDir, uploadId)
	if err := os.MkdirAll(uploadPath, 0755); err != nil {
		http.Error(w, "Failed to create upload directory", http.StatusInternalServerError)
		return
	}
	chunkPath := filepath.Join(uploadPath, fmt.Sprintf("chunk_%s", index))
	out, err := os.Create(chunkPath)
	if err != nil {
		http.Error(w, "Failed to save chunk", http.StatusInternalServerError)
		return
	}
	defer out.Close()
	if _, err := io.Copy(out, file); err != nil {
		http.Error(w, "Failed to write chunk to disk", http.StatusInternalServerError)
		return
	}
	log.Printf("✅ Received chunk %s for uploadId: %s", index, uploadId)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"message": fmt.Sprintf("Chunk %s for %s received.", index, uploadId),
	})
}

func finishHandler(w http.ResponseWriter, r *http.Request) {
	uploadId := r.FormValue("uploadId")
	filename := r.FormValue("filename")
	userhash := r.FormValue("userhash")
	destination := r.FormValue("destination")
	timeVal := r.FormValue("time")
	if timeVal == "" {
		timeVal = "1h"
	}
	if uploadId == "" || filename == "" || destination == "" {
		http.Error(w, "Missing uploadId, filename, or destination", http.StatusBadRequest)
		return
	}
	chunkDir := filepath.Join(uploadsDir, uploadId)
	defer os.RemoveAll(chunkDir)

	finalPath, err := assembleChunks(chunkDir, filename)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer os.Remove(finalPath)

	log.Printf("📦 Assembled file ready: %s", finalPath)
	processAndForwardFile(w, finalPath, filename, userhash, destination, timeVal)
}

func directHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		http.Error(w, "Invalid form: "+err.Error(), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, "Missing file", http.StatusBadRequest)
		return
	}
	defer file.Close()

	uniqueFilename := uuid.New().String() + filepath.Ext(header.Filename)
	tempPath := filepath.Join(tempDir, uniqueFilename)
	out, err := os.Create(tempPath)
	if err != nil {
		http.Error(w, "Failed to save temporary file", http.StatusInternalServerError)
		return
	}
	defer os.Remove(tempPath)

	size, err := io.Copy(out, file)
	if err != nil {
		out.Close()
		http.Error(w, "Failed to write file to disk", http.StatusInternalServerError)
		return
	}

	if err := out.Close(); err != nil {
		log.Printf("❌ Error closing temporary file %s: %v", tempPath, err)
		http.Error(w, "Failed to finalize temporary file", http.StatusInternalServerError)
		return
	}

	destination := r.FormValue("destination")
	timeVal := r.FormValue("time")
	if timeVal == "" {
		timeVal = "1h"
	}
	userhash := r.FormValue("userhash")

	log.Printf("📥 Direct upload received: %s → %s", header.Filename, destination)
	log.Printf("🚀 Uploading to: %s (%s)", destination, formatBytes(size))

	processAndForwardFile(w, tempPath, header.Filename, userhash, destination, timeVal)
}

func assembleChunks(chunkDir, filename string) (string, error) {
	entries, err := os.ReadDir(chunkDir)
	if err != nil {
		return "", fmt.Errorf("no chunks found for %s: %w", filepath.Base(chunkDir), err)
	}
	log.Printf("🔧 Reassembling chunks for uploadId %s...", filepath.Base(chunkDir))
	var chunks []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "chunk_") {
			chunks = append(chunks, e.Name())
		}
	}
	sort.Slice(chunks, func(i, j int) bool {
		ni, errI := strconv.Atoi(strings.TrimPrefix(chunks[i], "chunk_"))
		nj, errJ := strconv.Atoi(strings.TrimPrefix(chunks[j], "chunk_"))
		if errI != nil || errJ != nil {
			return false
		}
		return ni < nj
	})

	uniqueFinalName := fmt.Sprintf("%s-%s", uuid.New().String(), filename)
	finalPath := filepath.Join(tempDir, uniqueFinalName)
	finalFile, err := os.Create(finalPath)
	if err != nil {
		return "", fmt.Errorf("failed to create final file: %w", err)
	}
	defer finalFile.Close()

	for _, chunkName := range chunks {
		chunkPath := filepath.Join(chunkDir, chunkName)
		chunkFile, err := os.Open(chunkPath)
		if err != nil {
			return "", fmt.Errorf("error opening chunk %s for reading: %w", chunkName, err)
		}
		_, err = io.Copy(finalFile, chunkFile)
		chunkFile.Close()
		if err != nil {
			return "", fmt.Errorf("error writing chunk %s to final file: %w", chunkName, err)
		}
	}
	return finalPath, nil
}

func forwardToDestination(destination, filePath, filename, userhash, timeVal string) (string, error) {
	pipeReader, pipeWriter := io.Pipe()
	writer := multipart.NewWriter(pipeWriter)
	go func() {
		defer pipeWriter.Close()
		defer writer.Close()
		file, err := os.Open(filePath)
		if err != nil {
			pipeWriter.CloseWithError(fmt.Errorf("failed to open file for streaming: %w", err))
			return
		}
		defer file.Close()
		var part io.Writer
		switch destination {
		case "pomf":
			part, err = writer.CreateFormFile("files[]", filename)
		case "catbox", "litterbox":
			writer.WriteField("reqtype", "fileupload")
			if destination == "catbox" && userhash != "" {
				writer.WriteField("userhash", userhash)
			}
			if destination == "litterbox" {
				writer.WriteField("time", timeVal)
			}
			part, err = writer.CreateFormFile("fileToUpload", filename)
		default:
			pipeWriter.CloseWithError(fmt.Errorf("unknown destination: %s", destination))
			return
		}
		if err != nil {
			pipeWriter.CloseWithError(fmt.Errorf("failed to create form file part: %w", err))
			return
		}
		if _, err := io.Copy(part, file); err != nil {
			pipeWriter.CloseWithError(fmt.Errorf("failed to stream file content: %w", err))
			return
		}
	}()
	urlMap := map[string]string{
		"pomf":      "https://pomf.lain.la/upload.php",
		"catbox":    "https://catbox.moe/user/api.php",
		"litterbox": "https://litterbox.catbox.moe/resources/internals/api.php",
	}
	url, ok := urlMap[destination]
	if !ok {
		return "", fmt.Errorf("destination '%s' is not supported", destination)
	}
	req, err := http.NewRequest("POST", url, pipeReader)
	if err != nil {
		return "", fmt.Errorf("failed to create http request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http request failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(respBody))
	}
	if destination == "pomf" {
		var result struct {
			Success bool `json:"success"`
			Files   []struct {
				URL string `json:"url"`
			} `json:"files"`
			Error string `json:"error"`
		}
		if err := json.Unmarshal(respBody, &result); err != nil {
			return "", fmt.Errorf("failed to parse pomf response: %w", err)
		}
		if !result.Success {
			return "", fmt.Errorf("pomf upload failed: %s", result.Error)
		}
		if len(result.Files) > 0 {
			return result.Files[0].URL, nil
		}
		return "", fmt.Errorf("pomf response missing file URL")
	}
	return string(respBody), nil
}

func formatBytes(bytes int64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)
	switch {
	case bytes >= GB:
		return fmt.Sprintf("%.2f GB", float64(bytes)/float64(GB))
	case bytes >= MB:
		return fmt.Sprintf("%.2f MB", float64(bytes)/float64(MB))
	case bytes >= KB:
		return fmt.Sprintf("%.2f KB", float64(bytes)/float64(KB))
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}

func createProxyHandler(targetHost, stripPrefix string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		filePath := strings.TrimPrefix(r.URL.Path, stripPrefix)
		if filePath == "" {
			http.Error(w, "File path is missing.", http.StatusBadRequest)
			return
		}

		targetURL := targetHost + filePath
		log.Printf("Proxying %s to %s", r.URL.Path, targetURL)

		req, err := http.NewRequest(http.MethodGet, targetURL, nil)
		if err != nil {
			log.Printf("❌ Proxy error for %s (creating request): %v", targetURL, err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}

		if rangeHeader := r.Header.Get("Range"); rangeHeader != "" {
			req.Header.Set("Range", rangeHeader)
		}

		client := &http.Client{Timeout: 5 * time.Minute}
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("❌ Proxy error for %s (executing request): %v", targetURL, err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		for key, values := range resp.Header {
			w.Header()[key] = values
		}

		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

func main() {
	defer CloseDB()
	http.HandleFunc("/chunk", chunkHandler)
	http.HandleFunc("/finish", finishHandler)
	http.HandleFunc("/direct", directHandler)
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	http.HandleFunc("/catbox/", createProxyHandler("https://files.catbox.moe/", "/catbox/"))
	http.HandleFunc("/litterbox/", createProxyHandler("https://litter.catbox.moe/", "/litterbox/"))
	http.HandleFunc("/pomf/", createProxyHandler("https://pomf.lain.la/", "/pomf/"))
	http.HandleFunc("/", healthHandler)
	log.Println("✅ Server listening on http://localhost:3000")
	if err := http.ListenAndServe(":3000", nil); err != nil {
		log.Fatalf("❌ Server failed to start: %v", err)
	}
}