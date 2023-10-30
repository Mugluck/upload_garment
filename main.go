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
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/awsdocs/aws-doc-sdk-examples/gov2/s3/actions"
	"github.com/google/uuid"
	"github.com/qiniu/qmgo"
	"go.mongodb.org/mongo-driver/bson"
)

type MyEvent struct {
	Name   string `json:"name"`
	UserId string `json:"user_id"`
}

type Garment struct {
	Id            string `json:"id"`
	Name          string `json:"name"`
	User_Id       string `json:"user_id"`
	Material_Flag bool   `json:"material_flag"`
	Model_Flag    bool   `json:"model_flag"`
	Metadata_Flag bool   `json:"metadata_flag"`
	Render_Flag   bool   `json:"render_flag"`
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

var db = client.Database("drape_manager")
var client, err = qmgo.NewClient(context.Background(), &qmgo.Config{Uri: uri})

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

type File struct {
	Id            string `json:"id"`
	FileName      string `json:"file_name"`
	FileExtension string `json:"file_extension"`
	Url           string `json:"url"`
	Bucket        string `json:"bucket"`
	Fileable_Type string `json:"fileable_type"`
	Fileable_Id   string `json:"fileable_id"`
	Name          string `json:"name"`
	Key           string `json:"key"`
	Disk          string `json:"disk"`
	Type          int64  `json:"type"`
	PresignedURL  string `json:"presigned_url"`
}

type Input struct {
	FileName      string `json:"file_name"`
	FileExtension string `json:"file_extension"`
}

func HandleRequest(ctx context.Context, request events.APIGatewayProxyRequest) (events.APIGatewayProxyResponse, error) {

	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Could not connect to db: " + err.Error(),
		}, nil
	}

	userId := request.PathParameters["user_id"]
	input := Input{}
	json.Unmarshal([]byte(request.Body), &input)

	users := db.Collection("users")

	user := User{}
	err = users.Find(context.Background(), bson.M{"id": userId}).One(&user)

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
	garments := db.Collection("garments")

	garment := Garment{}
	err = garments.Find(context.Background(), bson.M{"id": garmentId}).One(&garment)

	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Could not find garment. Data: " + garmentId + " " + err.Error(),
		}, nil
	}

	fileName := input.FileName
	fileCategory := setUpdateFile(fileName)
	if fileCategory == "unknown" {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "File type not supported.",
		}, nil
	}

	extension := input.FileExtension
	folder := "garment/" + garmentId + "/original/"
	configS3()
	presignClient := s3.NewPresignClient(awsS3Client)
	presigner := actions.Presigner{PresignClient: presignClient}
	presignedPutRequest, err := presigner.PutObject(awsBucket, folder+fileName, 600)
	// err = uploadFile(part, fileName, folder+fileName)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Upload failed : " + err.Error(),
		}, nil
	}
	// files are saved, we need to get the s3 info and the file info and save it against the garment
	// to download the file, we need the bucket, the filename, and the url ( e.g. s3://drape-images//garment/garment_0000016_L_button.fbx.fbx)
	file := File{
		Id:            uuid.New().String(),
		FileName:      fileName,
		FileExtension: extension,
		Url:           "s3://" + awsBucket + folder + fileName,
		Bucket:        awsBucket,
		Fileable_Type: "garment",
		Fileable_Id:   garmentId,
		Name:          fileNameWithoutExtSliceNotation(fileName),
		Key:           folder + fileName,
		Disk:          "s3",
		Type:          5,
		PresignedURL:  presignedPutRequest.URL,
	}

	files := client.Database("drape_manager").Collection("files")
	_, insertErr := files.InsertOne(context.TODO(), file)
	// info, err = garments.Find(context.Background(), bson.M{"id": garmentId}).Apply(change, &doc)
	if insertErr != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "File db saving failed : " + insertErr.Error(),
		}, nil
	}

	//update garment to mark file upload status
	err = garments.UpdateOne(context.Background(), bson.M{"id": garmentId}, bson.M{"$set": bson.M{fileCategory: true}})
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "garment updating failed : " + insertErr.Error(),
		}, nil
	}

	// update the garment to mark file link
	fileCategoryId := fileCategory + "_id"
	err = garments.UpdateOne(context.Background(), bson.M{"id": garmentId}, bson.M{"$set": bson.M{fileCategoryId: file.Id}})

	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 500,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "garment updating failed : " + insertErr.Error(),
		}, nil
	}

	customBytes, err := json.Marshal(file)
	if err != nil {
		return events.APIGatewayProxyResponse{
			StatusCode: 400,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Marshall json data failed : " + err.Error(),
		}, nil
	}

	garments.Find(context.Background(), bson.M{"id": garmentId}).One(&garment)
	if checkIfReady(garment) {
		// kick off job
		return events.APIGatewayProxyResponse{
			StatusCode: 200,
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: "Garment ready: " + string(customBytes),
		}, nil
	}

	return events.APIGatewayProxyResponse{
		StatusCode: 200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: "File result: " + string(customBytes),
	}, nil
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

// func uploadFile(data io.Reader, fileName string, key string) error {

// 	// Get a file from the form input name "file"
// 	// get file body
// 	// file := getReader(path + fileName)
// 	// reader := io.Reader(bytes.NewReader(data))

// 	uploader := manager.NewUploader(awsS3Client)
// 	_, err := uploader.Upload(context.TODO(), &s3.PutObjectInput{
// 		Bucket: aws.String(awsBucket),
// 		Key:    aws.String(key),
// 		Body:   data,
// 	})

// 	return err
// }

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

func fileNameWithoutExtSliceNotation(fileName string) string {
	return fileName[:len(fileName)-len(filepath.Ext(fileName))]
}

func setUpdateFile(fileName string) string {

	if fileName == "start_meshes.mtl" {
		return "material_flag"
	}
	if fileName == "start_meshes.obj" {
		return "model_flag"
	}
	if fileName == "start_meshes_meta_data.xml" {
		return "metadata_flag"
	}
	if fileName == "render_meshes.fbx" {
		return "render_flag"
	}
	return "unknown"
}

func checkIfReady(garment Garment) bool {

	if garment.Material_Flag && garment.Model_Flag && garment.Metadata_Flag && garment.Render_Flag {
		return true
	} else {
		return false
	}
}

// Presigner encapsulates the Amazon Simple Storage Service (Amazon S3) presign actions
// used in the examples.
// It contains PresignClient, a client that is used to presign requests to Amazon S3.
// Presigned requests contain temporary credentials and can be made from any HTTP client.
type Presigner struct {
	PresignClient *s3.PresignClient
}

// PutObject makes a presigned request that can be used to put an object in a bucket.
// The presigned request is valid for the specified number of seconds.
func (presigner Presigner) PutObject(
	bucketName string, objectKey string, lifetimeSecs int64) (*v4.PresignedHTTPRequest, error) {
	request, err := presigner.PresignClient.PresignPutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(bucketName),
		Key:    aws.String(objectKey),
	}, func(opts *s3.PresignOptions) {
		opts.Expires = time.Duration(lifetimeSecs * int64(time.Second))
	})
	if err != nil {
		log.Printf("Couldn't get a presigned request to put %v:%v. Here's why: %v\n",
			bucketName, objectKey, err)
	}
	return request, err
}
