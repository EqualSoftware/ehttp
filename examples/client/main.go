package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/EqualSoftware/ehttp"
)

func main() {
	client := ehttp.NewClient(
		ehttp.WithBaseUrl("http://pudim.com.br"),
		ehttp.WithTimeout(5*time.Second),
		ehttp.WithAuthorization("my-auth-token"),
	)

	response, err := client.Execute(
		ehttp.WithContext(context.Background()),
		ehttp.WithSource("GET", "/?nocache=123&test=123123"),
		ehttp.WithHeader("Accept", "text/html"),
	)

	if err != nil {
		log.Fatalln(err)
	}

	data, err := response.String()

	if err != nil {
		log.Fatalln(err)
	}

	fmt.Printf("# URL: %s\n\n", response.Request.URL.String())
	fmt.Printf("# Status: %d\n\n# Body:\n\n%s\n", response.StatusCode, data)
}
