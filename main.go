package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/grokify/go-awslambda"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type MyEvent struct {
	Name   string `json:"name"`
	UserId string `json:"user_id"`
}

type Garment struct {
	Id      string `json:"id"`
	Name    string `json:"name"`
	User_Id string `json:"user_id"`
}

type User struct {
	Id   string `json:"id"`
	Name string `json:"name"`
}

var ExposeServerErrors = true
var uri = os.Getenv("ATLAS_URI")
var awsBucket = os.Getenv("AWS_BUCKET")
var awsS3Client *s3.Client
var awsAccessKeyID = os.Getenv("KEY")
var awsSecretAccessKey = os.Getenv("SECRET")
var awsRegion = os.Getenv("REGION")
var client, err = mongo.Connect(context.Background(), options.Client().ApplyURI(uri))

//endpoints
//new-garment -> user/{id}/
//user/{id}/garment/{id}/file
// when final file is uploaded, sends off job to manager

// HTTPError is a generic struct type for JSON error responses. It allows the
// library to assign an HTTP status code for the errors returned by its various
// functions.
type HTTPError struct {
	Status  int    `json:"code"`
	Message string `json:"message"`
}

type customStruct struct {
	Content       string
	FileName      string
	FileExtension string
}

func HandleRequest(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {

	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Could not connect to db.",
		}, nil
	}

	userId := request.PathParameters["user_id"]
	users := client.Database("drape_manager").Collection("users")

	userSearch := users.FindOne(context.Background(), bson.M{"id": userId})
	user := User{}
	err := userSearch.Decode(&user)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Could not find user. Data: " + userId,
		}, nil
	}

	garmentId := request.PathParameters["garment_id"]
	garments := client.Database("drape_manager").Collection("garments")
	garmentSearch := garments.FindOne(context.Background(), bson.M{"id": garmentId})
	garment := Garment{}
	err = garmentSearch.Decode(&garment)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Could not find garment. Data: " + garmentId,
		}, nil
	}

	// now that we've established the db data, we can start uploading the file
	r, err := awslambda.NewReaderMultipart(request)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Reading failed : " + err.Error(),
		}, nil
	}

	part, err := r.NextPart()
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Part failed : " + err.Error(),
		}, nil
	}

	// content, err := io.ReadAll(part)
	// if err != nil {
	// 	return events.APIGatewayProxyResponse{
	// 		StatusCode: 400,
	// 		Headers: map[string]string{
	// 			"Content-Type": "application/json",
	// 		},
	// 		Body: "Get Content failed : " + err.Error(),
	// 	}, nil
	// }

	configS3()
	err = uploadFile(part, part.FileName(), "/garment/"+part.FileName()+filepath.Ext(part.FileName()))
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Upload failed : " + err.Error(),
		}, nil
	}

	custom := customStruct{
		Content:       string(""),
		FileName:      part.FileName(),
		FileExtension: filepath.Ext(part.FileName())}

	customBytes, err := json.Marshal(custom)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Marshall json data failed : " + err.Error(),
		}, nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: "File result: " + string(customBytes),
	}, nil

	// _, insertErr := coll.InsertOne(context.TODO(), garment)

	// if insertErr != nil {
	// 	return "", err
	// }

}

func main() {
	lambda.Start(HandleRequest)
}

const uploadLimitBytes = 50000000 // 50 megabytes

type UploadResponse struct {
	Concat string
}

func UploadSessionsLambda(_ context.Context, lambdaReq events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {
	contentType := lambdaReq.Headers["Content-Type"]
	if contentType == "" {
		return HandleHTTPError(http.StatusBadRequest, fmt.Errorf("request contained no Content-Type header"))
	}

	_, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return HandleHTTPError(http.StatusBadRequest, err)
	}

	boundary := params["boundary"]
	if boundary == "" {
		return HandleHTTPError(http.StatusBadRequest, fmt.Errorf("request contained no boundary value to parse from Content-Type headers"))
	}

	stringReader := strings.NewReader(lambdaReq.Body)
	multipartReader := multipart.NewReader(stringReader, boundary)

	form, err := multipartReader.ReadForm(uploadLimitBytes)
	if err != nil {
		return HandleHTTPError(http.StatusBadRequest, err)
	}

	var sb strings.Builder

	for currentFileName := range form.File {
		// anonymous file handler func allows for calling defer .Close()
		httpStatus, handlerErr := func(fileName string) (int, error) {
			currentFileHeader := form.File[currentFileName][0]
			currentFile, openErr := currentFileHeader.Open()
			if openErr != nil {
				return http.StatusInternalServerError, openErr
			}

			defer currentFile.Close() // figure out how to trap this error

			bufferedReader := bufio.NewReader(currentFile)

			for {
				line, _, readLineErr := bufferedReader.ReadLine()
				if readLineErr == io.EOF {
					break
				}
				sb.Write(line)
			}

			return http.StatusOK, nil
		}(currentFileName)

		if handlerErr != nil {
			return HandleHTTPError(httpStatus, handlerErr)
		}
	}

	return MarshalSuccess(&UploadResponse{Concat: sb.String()})
}

func HandleHTTPError(httpStatus int, err error) (events.APIGatewayProxyResponse, error) {
	httpErr := HTTPError{
		Status:  httpStatus,
		Message: err.Error(),
	}

	if httpErr.Status >= 500 && !ExposeServerErrors {
		httpErr.Message = http.StatusText(httpErr.Status)
	}

	return MarshalResponse(httpErr.Status, nil, httpErr)
}

func MarshalSuccess(data interface{}) (events.APIGatewayProxyResponse, error) {
	return MarshalResponse(http.StatusOK, nil, data)
}

// MarshalResponse generated an events.APIGatewayProxyResponse object that can
// be directly returned via the lambda's handler function. It receives an HTTP
// status code for the response, a map of HTTP headers (can be empty or nil),
// and a value (probably a struct) representing the response body. This value
// will be marshaled to JSON (currently without base 64 encoding).
func MarshalResponse(httpStatus int, headers map[string]string, data interface{}) (
	events.APIGatewayProxyResponse,
	error,
) {
	b, err := json.Marshal(data)
	if err != nil {
		httpStatus = http.StatusInternalServerError
		b = []byte(`{"code":500,"message":"the server has encountered an unexpected error"}`)
	}

	if headers == nil {
		headers = make(map[string]string)
	}

	return events.APIGatewayProxyResponse{
		StatusCode:      httpStatus,
		IsBase64Encoded: false,
		Headers:         headers,
		Body:            string(b),
	}, nil
}

func uploadFile(data io.Reader, fileName string, key string) error {

	// Get a file from the form input name "file"
	// get file body
	// file := getReader(path + fileName)
	// reader := io.Reader(bytes.NewReader(data))

	uploader := manager.NewUploader(awsS3Client)
	_, err := uploader.Upload(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(awsBucket),
		Key:    aws.String(key),
		Body:   data,
	})

	return err
}

// configS3 creates the S3 client
func configS3() {

	// cfg, err := config.LoadDefaultConfig(context.TODO(), config.WithRegion(AWS_S3_REGION), config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("etest", "testw", "")))

	cfg, err := config.LoadDefaultConfig(context.TODO(),
		config.WithRegion(awsRegion),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(awsAccessKeyID, awsSecretAccessKey, "")),
	)
	if err != nil {
		log.Fatal(err)
	}

	awsS3Client = s3.NewFromConfig(cfg)
}
