package gateway

import "github.com/versity/versitygw/s3err"

func invalidArgument(message string) error {
	return s3err.APIError{Code: "InvalidArgument", Description: message, HTTPStatusCode: 400}
}

func insufficientStorage() error {
	return s3err.APIError{Code: "InsufficientStorage", Description: "Insufficient upload staging storage", HTTPStatusCode: 507}
}
