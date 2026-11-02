package main

import (
	"context"
	"time"

	"github.com/EqualSoftware/ehttp/examples/server/app"
	"go.uber.org/zap"
)

func main() {
	// The server owns its logger. This one is for the failure of main only.
	logger := zap.Must(zap.NewDevelopment())

	if err := app.New("./examples/server", time.Now()).Listen(context.Background()); err != nil {
		logger.Fatal("server stopped", zap.Error(err))
	}
}
