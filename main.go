// 命令 metadata-lineage-catalog 启动一个纯本地、仅依赖 Go 标准库的
// 元数据目录 HTTP 服务。数据落在本地目录,不连接任何外部系统。
package main

import (
	"flag"
	"log"

	"github.com/lechandonga/metadata-lineage-catalog-go/catalog"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP 监听地址")
	dataDir := flag.String("data-dir", "./data", "本地数据目录(WAL 与快照)")
	flag.Parse()

	store, err := catalog.OpenFileStore(*dataDir)
	if err != nil {
		log.Fatalf("打开本地存储失败: %v", err)
	}
	defer store.Close()

	c, err := catalog.New(store)
	if err != nil {
		log.Fatalf("加载元数据目录失败(数据可能已损坏): %v", err)
	}

	server := catalog.NewServer(c, log.Default())
	if err := server.ListenAndServe(*addr); err != nil {
		log.Fatal(err)
	}
}
