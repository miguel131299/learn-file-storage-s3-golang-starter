package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const uploadLimit = 1 << 30

	// extract videoID
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// authenticate user
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

	// get video metadata
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

	// Authorize user
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Unauthorized to upload video", err)
		return
	}

	// "video" should match the HTML form input name
	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	// check content type
	contentType := header.Header.Get("Content-Type")
	mediatype, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Error parsing media type", err)
		return
	}

	if !(mediatype == "video/mp4") {
		respondWithError(w, http.StatusBadRequest, "Media type is not allowed", nil)
		return
	}

	// generate file name
	fileExtension := strings.Split(contentType, "/")[1]
	buffer := make([]byte, 32)
	_, err = rand.Read(buffer)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating random file name", err)
	}
	fileName := base64.RawURLEncoding.EncodeToString(buffer) + "." + fileExtension

	// Save uploaded file to disk
	originalFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error creating file", nil)
		return
	}
	_, err = io.Copy(originalFile, file)
	if err != nil {
		originalFile.Close()
		os.Remove(originalFile.Name())
		respondWithError(w, http.StatusInternalServerError, "Error copying data to file", err)
		return
	}
	originalFile.Close()

	// Process video for fast start
	processedPath, err := processVideoForFastStart(originalFile.Name())
	if err != nil {
		os.Remove(originalFile.Name())
		respondWithError(w, http.StatusInternalServerError, "Error processing video for fast start", err)
		return
	}
	defer os.Remove(originalFile.Name()) // delete original
	defer os.Remove(processedPath)       // delete processed

	// Open processed file for upload
	// we do this to avoid sending 3 request at the start of playing a video
	processedFile, err := os.Open(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error opening processed video", err)
		return
	}
	defer processedFile.Close()

	// Determine video orientation
	orientation, err := getVideoAspectRatio(processedPath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error getting aspect ratio", err)
		return
	}
	objKey := orientation + "/" + fileName

	// Upload to S3
	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &objKey,
		ContentType: &mediatype,
		Body:        processedFile,
	})
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Failed to upload video to S3", err)
		return
	}

	// Update video metadata
	// videoURL := fmt.Sprintf("https://%s.s3.%s.amazonaws.com/%s", cfg.s3Bucket, cfg.s3Region, objKey)
	videoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, objKey)
	video.VideoURL = &videoURL

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error updating video metadata", err)
		return
	}

	// generate Presigned URL
	signedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error generating signed video URL when uploading", err)
		return
	}
	respondWithJSON(w, http.StatusOK, signedVideo)
}

type ffprobeOutput struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
}

// approx checks if two floats are close enough
func approx(a, b float64) bool {
	const tolerance = 0.05
	return (a > b-tolerance) && (a < b+tolerance)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-show_streams", filePath)

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to run ffprobe: %w", err)
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(out.Bytes(), &parsed); err != nil {
		return "", fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	if len(parsed.Streams) == 0 {
		return "", errors.New("no streams found in video")
	}

	width := parsed.Streams[0].Width
	height := parsed.Streams[0].Height

	if width == 0 || height == 0 {
		return "", errors.New("invalid width or height")
	}

	ratio := float64(width) / float64(height)

	switch {
	case approx(ratio, 16.0/9.0):
		return "landscape", nil
	case approx(ratio, 9.0/16.0):
		return "portrait", nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outputPath := filePath + ".processing"

	cmd := exec.Command(
		"ffmpeg",
		"-i", filePath,
		"-c", "copy",
		"-movflags", "faststart",
		"-f", "mp4",
		outputPath,
	)

	// Optional: print output for debugging
	cmd.Stdout = nil
	cmd.Stderr = nil

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("failed to process video for fast start: %w", err)
	}

	return outputPath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	// Create a presign client from the standard S3 client
	presignClient := s3.NewPresignClient(s3Client)

	// Prepare the input for the presigned GET request
	input := &s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}

	// Generate the presigned URL
	req, err := presignClient.PresignGetObject(context.TODO(), input,
		s3.WithPresignExpires(expireTime),
	)
	if err != nil {
		return "", fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	return req.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	// Expecting VideoURL to be in the format "bucket,key"
	parts := strings.Split(*video.VideoURL, ",")
	if len(parts) != 2 {
		return video, fmt.Errorf("invalid video URL format; expected 'bucket,key'")
	}

	bucket := parts[0]
	key := parts[1]

	// Generate a presigned URL
	signedURL, err := generatePresignedURL(cfg.s3Client, bucket, key, 15*time.Minute)
	if err != nil {
		return video, fmt.Errorf("failed to generate presigned URL: %w", err)
	}

	// Update video with the signed URL
	video.VideoURL = &signedURL
	return video, nil
}
