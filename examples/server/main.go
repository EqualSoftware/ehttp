package main

import (
	"context"
	"time"

	"github.com/EqualSoftware/ehttp/examples/server/app"
	"go.uber.org/zap"
)

func init() {
	logger := zap.Must(zap.NewDevelopment())
	zap.ReplaceGlobals(logger)
}

func main() {
	if err := app.New("./examples/server", time.Now()).Listen(context.Background()); err != nil {
		zap.L().Fatal("server stopped", zap.Error(err))
	}
}
