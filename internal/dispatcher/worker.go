package dispatcher

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"simple-go-app/internal/envHelper"
	"simple-go-app/internal/parsing"
	"simple-go-app/internal/store"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/sqs"
)

var (
	lastRequestTime   time.Time
	lastRequestTimeMu sync.Mutex
)

func Worker(id int, messageQueue <-chan *sqs.Message, svc *sqs.SQS, sqsURL, s3Bucket string, s *store.Store) {
	awsRegion := envHelper.GetEnvVariable("AWS_REGION")
	minGapBetweenRequests := envHelper.GetEnvVariable("MINIMUM_GAP_BETWEEN_REQUESTS_SECONDS")
	minGap, err := time.ParseDuration(minGapBetweenRequests + "s")
	if err != nil {
		log.Fatalf("Error parsing MINIMUM_GAP_BETWEEN_REQUESTS_SECONDS: %v", err)
	}

	log.Printf("Starting worker %d...\n", id)

	for {
		message := <-messageQueue
		processMessage(id, message, svc, sqsURL, s3Bucket, awsRegion, minGap)
	}
}

func createAWSSession(region string) *session.Session {
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	return sess
}

func downloadFileFromS3(s3Svc *s3.S3, bucket, path string) ([]byte, error) {
	output, err := s3Svc.GetObject(&s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(path),
	})
	if err != nil {
		return nil, err
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			fmt.Println("Error closing S3 response body:", err)
		}
	}(output.Body)

	fileContent, err := ioutil.ReadAll(output.Body)
	if err != nil {
		return nil, err
	}
	return fileContent, nil
}

func processMessage(id int, message *sqs.Message, svc *sqs.SQS, sqsURL, s3Bucket, awsRegion string, minGap time.Duration) {
	var msgData map[string]interface{}
	if err := json.Unmarshal([]byte(*message.Body), &msgData); err != nil {
		log.Println("Error decoding JSON message:", err)
		return
	}

	path := msgData["s3Location"].(string)
	userID := msgData["user_id"].(string)
	screenID := msgData["screen_id"].(string)

	fmt.Printf("Worker %d received message. Path: %s. User ID: %s. Screen ID: %s\n", id, path, userID, screenID)

	lastRequestTimeMu.Lock()
	timeSinceLastRequest := time.Since(lastRequestTime)
	lastRequestTimeMu.Unlock()

	if timeSinceLastRequest < minGap {
		sleepTime := minGap - timeSinceLastRequest
		log.Printf("Worker %d sleeping for %v to meet the minimum gap between requests\n", id, sleepTime)
		time.Sleep(sleepTime)
	}

	sess := createAWSSession(awsRegion)
	s3Svc := s3.New(sess)

	fileContent, err := downloadFileFromS3(s3Svc, s3Bucket, path)
	if err != nil {
		log.Println("Error downloading file from S3:", err)
		log.Printf("Bucket: %s, Key: %s\n", s3Bucket, path)
		return
	}

	CrudeGrobidResponse, err := parsing.SendPDF2Grobid(fileContent)
	if err != nil {
		log.Println("Error sending file to Grobid service:", err)
		return
	}

	// clean up grobid response
	tidyGrobidResponse, err := parsing.TidyUpGrobidResponse(CrudeGrobidResponse)
	if err != nil {
		log.Println("Error tidying up Grobid response:", err)
		return
	}

	// cross reference data using the DOI
	crudeCrossRefResponse, err := parsing.CrossReferenceData(tidyGrobidResponse.Doi)
	if err != nil {
		log.Println("Error cross referencing data:", err)
		return
	}

	// tidy up cross referenced data
	_ = parsing.TidyCrossRefData(crudeCrossRefResponse)

	// give preference to crossref data

	lastRequestTimeMu.Lock()
	lastRequestTime = time.Now()
	lastRequestTimeMu.Unlock()

	_, err = svc.ChangeMessageVisibility(&sqs.ChangeMessageVisibilityInput{
		QueueUrl:          aws.String(sqsURL),
		ReceiptHandle:     message.ReceiptHandle,
		VisibilityTimeout: aws.Int64(30),
	})
	if err != nil {
		log.Println("Error putting message back to the queue:", err)
	}
}
