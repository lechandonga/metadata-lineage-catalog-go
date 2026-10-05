// Command catalog 启动本地元数据目录服务。
//
// 用法:
//
//	go run ./cmd/catalog -addr :8080 -data ./data/catalog-state.json
//
// 不依赖任何外部系统：状态落本地文件，HTTP 提供查询与登记入口。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/lechandonga/metadata-lineage-catalog-go/internal/catalog"
	"github.com/lechandonga/metadata-lineage-catalog-go/internal/httpapi"
)

func main() {
	addr := flag.String("addr", envOr("CATALOG_ADDR", "127.0.0.1:8080"), "HTTP 监听地址")
	data := flag.String("data", envOr("CATALOG_DATA", "./data/catalog-state.json"), "本地状态文件路径")
	flag.Parse()

	store := catalog.NewFileStore(*data)
	cat, err := catalog.New(store, time.Now)
	if err != nil {
		log.Fatalf("打开元数据目录失败（状态文件可能已损坏）: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           httpapi.New(cat),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("metadata catalog listening on http://%s (state: %s)", *addr, *data)
		errCh <- srv.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务退出: %v", err)
		}
	case <-stop:
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("优雅关闭失败: %v", err)
		}
		log.Println("服务已停止，状态文件保持一致")
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
