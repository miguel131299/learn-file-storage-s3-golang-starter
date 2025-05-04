package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadThumbnail(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	fmt.Println("uploading thumbnail for video", videoID, "by user", userID)

	// TODO: implement the upload here
	const maxMemory = 10 << 20
	r.ParseMultipartForm(maxMemory)

	// "thumbnail" should match the HTML form input name
	file, header, err := r.FormFile("thumbnail")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// No chirp found
			respondWithError(w, http.StatusNotFound, "No video with videoID", err)
			return
		}

		respondWithError(w, http.StatusInternalServerError, "Could not read file", err)
		return
	}

	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized to upload thumbnail", nil)
		return
	}

	// check content type
	contentType := header.Header.Get("Content-Type")
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing media type", err)
		return
	}

	if !(mediatype == "image/jpeg" || mediatype == "image/png") {
		respondWithError(w, http.StatusBadRequest, "Media type is not allowed", nil)
		return
	}

	// generate file path
	fileExtension := strings.Split(contentType, "/")[1]
	buffer := make([]byte, 32)
	_, err = rand.Read(buffer)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating random file name", err)
	}
	fileName := base64.RawURLEncoding.EncodeToString(buffer) + "." + fileExtension
	filepath := filepath.Join(cfg.assetsRoot, fileName)

	// create file on filesystem
	osFile, err := os.Create(filepath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating file", nil)
		return
	}

	// copy data to os file
	_, err = io.Copy(osFile, file)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error copying data to file", err)
		return
	}

	// update video metadata
	thumbnail_url := fmt.Sprintf("http://localhost:%s/assets/%s", cfg.port, fileName)
	video.ThumbnailURL = &thumbnail_url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error storing video metadata", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}
