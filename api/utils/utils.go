package utils

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Given bytes and an S3 location write a file on S3 with expiration policy
// 0 value for expDays means no expiry
// If failure occurs append error message to the logs stream
// This function does not panic to safeguard server
func WriteToS3(svc *s3.S3, b []byte, key string, contType string, expDays int) error {

	var expirationDate *time.Time
	if expDays != 0 {
		expDate := time.Now().AddDate(0, 0, expDays)
		expirationDate = &expDate
	}

	// Upload the data to S3
	_, err := svc.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(os.Getenv("STORAGE_BUCKET")),
		Key:         aws.String(key),
		Body:        bytes.NewReader(b),
		Expires:     expirationDate,
		ContentType: &contType,
	})

	if err != nil {
		// to do log error
		return err
	}
	// to do log
	return nil
}

// Check if an S3 Key exists
func KeyExists(key string, svc *s3.S3) (bool, error) {
	_, err := svc.HeadObject(&s3.HeadObjectInput{
		Bucket: aws.String(os.Getenv("STORAGE_BUCKET")),
		Key:    aws.String(key),
	})

	if err != nil {
		if aerr, ok := err.(awserr.Error); ok {
			switch aerr.Code() {
			case "NotFound", "Forbidden": // s3.ErrCodeNoSuchKey does not work, aws is missing this error code so we hardwire a string
				return false, nil
			default:
				return false, err
			}
		}
		return false, err
	}

	return true, nil
}

// Check if a string is in string slice
func StringInSlice(a string, list []string) bool {
	for _, b := range list {
		if b == a {
			return true
		}
	}
	return false
}

// Assumes file exist
func GetS3JsonData(key string, svc *s3.S3) (interface{}, error) {
	// Create a new S3GetObjectInput object to specify the file you want to read
	params := &s3.GetObjectInput{
		Bucket: aws.String(os.Getenv("STORAGE_BUCKET")),
		Key:    aws.String(key),
	}

	// Use the S3 service object to download the file into a byte slice
	resp, err := svc.GetObject(params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Read the file contents into a byte slice
	jsonBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// Declare an empty interface{} value to hold the unmarshalled data
	var data interface{}

	// Unmarshal the JSON data into the interface{} value
	err = json.Unmarshal(jsonBytes, &data)
	if err != nil {
		return nil, err
	}

	return data, nil
}

// Assumes file exist
func GetS3LinesData(key string, svc *s3.S3) ([]string, error) {
	// Create a new S3GetObjectInput object to specify the file you want to read
	params := &s3.GetObjectInput{
		Bucket: aws.String(os.Getenv("STORAGE_BUCKET")),
		Key:    aws.String(key),
	}

	// Use the S3 service object to download the file into a byte slice
	resp, err := svc.GetObject(params)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var lines []string
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return lines, nil
}

var validTagPattern = regexp.MustCompile(`^[A-Za-z0-9._\-:]+$`)

// SanitizeTags trims whitespace from each tag in-place and validates that
// all tags are non-empty and contain only allowed characters (letters, numbers, '.', '-', '_', ':').
func SanitizeTags(tags []string) error {
	for i, tag := range tags {
		tag = strings.TrimSpace(tag)
		tags[i] = tag

		if tag == "" {
			return fmt.Errorf("tags cannot contain empty values")
		}

		if !validTagPattern.MatchString(tag) {
			return fmt.Errorf("tag %q contains unsupported characters. Allowed characters are letters, numbers, '.', '-', '_', and ':'", tag)
		}
	}
	return nil
}
