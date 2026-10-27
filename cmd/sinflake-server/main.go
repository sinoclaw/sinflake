// sinflake-server 是一个对齐美团 Leaf HTTP 接口的发号服务，纯标准库实现。
//
// 默认用内存号段（重启后号段从头开始，仅供联调/压测）；生产请在自己的进程里
// 用 sinflake.MySQLStore 接库，或用 -dsn 自行扩展（本服务不绑定任何 SQL 驱动）。
//
// 接口（与 Leaf 一致）：
//
//	GET /api/segment/get/{key}              号段取号，返回纯文本 ID
//	GET /api/segment/get/{key}/batch/{n}    批量取号，逗号分隔
//	GET /api/snowflake/get/{key}            snowflake 取号
//	GET /api/segment/cache                  号段缓存快照（JSON，Leaf 的 /cache）
//	GET /healthz                            健康检查
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sinoclaw/sinflake"
)

func main() {
	var (
		addr    = flag.String("addr", ":8080", "监听地址")
		mode    = flag.String("mode", "both", "启用模式：segment | snowflake | both")
		step    = flag.Int64("step", 2000, "内存号段步长（Leaf 默认 2000）")
		machine = flag.Int("machine", 1, "snowflake 机器号 0~65535")
		start   = flag.String("start", "2025-01-01T00:00:00Z", "snowflake 纪元（RFC3339）")
		keys    = flag.String("keys", "order,pay", "内存号段要预置的 key，逗号分隔")
	)
	flag.Parse()

	ctx := context.Background()

	// —— snowflake ——
	var sf *sinflake.Sinflake
	if *mode == "snowflake" || *mode == "both" {
		epoch, err := time.Parse(time.RFC3339, *start)
		if err != nil {
			log.Fatalf("bad -start: %v", err)
		}
		sf, err = sinflake.New(sinflake.Settings{
			StartTime: epoch,
			MachineID: func() (uint16, error) { return uint16(*machine), nil },
		})
		if err != nil {
			log.Fatalf("init snowflake: %v", err)
		}
	}

	// —— segment（内存版）——
	var gen *sinflake.SegmentGenerator
	if *mode == "segment" || *mode == "both" {
		var rows []sinflake.Alloc
		for _, k := range strings.Split(*keys, ",") {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			rows = append(rows, sinflake.Alloc{Key: k, MaxID: 1, Step: *step, Description: "created by sinflake-server"})
		}
		var err error
		gen, err = sinflake.NewSegmentGenerator(sinflake.NewMemStore(rows...))
		if err != nil {
			log.Fatalf("init segment: %v", err)
		}
		if err := gen.Init(ctx); err != nil {
			log.Fatalf("init segment cache: %v", err)
		}
		defer gen.Close()
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "ok")
	})

	// Leaf 风格：/api/segment/get/{key}
	mux.HandleFunc("/api/segment/get/", func(w http.ResponseWriter, r *http.Request) {
		if gen == nil {
			http.Error(w, "segment mode disabled", http.StatusNotFound)
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, "/api/segment/get/")
		parts := strings.Split(rest, "/")
		key := parts[0]
		if key == "" {
			noKey(w)
			return
		}
		// 可选 /batch/{n}
		if len(parts) == 3 && parts[1] == "batch" {
			n, err := strconv.Atoi(parts[2])
			if err != nil {
				http.Error(w, "bad batch size", http.StatusBadRequest)
				return
			}
			ids, err := gen.NextIDs(r.Context(), key, n)
			if err != nil {
				writeErr(w, r, err)
				return
			}
			ss := make([]string, len(ids))
			for i, id := range ids {
				ss[i] = strconv.FormatInt(id, 10)
			}
			fmt.Fprint(w, strings.Join(ss, ","))
			return
		}
		id, err := gen.NextID(r.Context(), key)
		if err != nil {
			writeErr(w, r, err)
			return
		}
		fmt.Fprint(w, id)
	})

	// Leaf 风格：/api/snowflake/get/{key}
	mux.HandleFunc("/api/snowflake/get/", func(w http.ResponseWriter, r *http.Request) {
		if sf == nil {
			http.Error(w, "snowflake mode disabled", http.StatusNotFound)
			return
		}
		if strings.TrimPrefix(r.URL.Path, "/api/snowflake/get/") == "" {
			noKey(w)
			return
		}
		id, err := sf.NextID()
		if err != nil {
			writeErr(w, r, err)
			return
		}
		fmt.Fprint(w, id)
	})

	// Leaf 的监控页 /cache，这里返回 JSON
	mux.HandleFunc("/api/segment/cache", func(w http.ResponseWriter, r *http.Request) {
		if gen == nil {
			http.Error(w, "segment mode disabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{
			"tags": gen.Cache(),
			"now":  time.Now().Format(time.RFC3339),
		})
	})

	// Leaf 的监控页是 HTML（Thymeleaf 模板 segment），这里给同形状的极简 HTML
	mux.HandleFunc("/cache", func(w http.ResponseWriter, r *http.Request) {
		if gen == nil {
			http.Error(w, "segment mode disabled", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><head><title>sinflake cache</title></head><body><h3>号段缓存</h3><table border=1 cellpadding=6>")
		fmt.Fprint(w, "<tr><th>key<th>current<th>max<th>step<th>idle<th>nextReady<th>updating</tr>")
		for _, v := range gen.Cache() {
			fmt.Fprintf(w, "<tr><td>%s<td>%d<td>%d<td>%d<td>%d<td>%v<td>%v</tr>",
				v.Key, v.CurrentID, v.MaxID, v.Step, v.Idle, v.NextReady, v.Updating)
		}
		fmt.Fprint(w, "</table></body></html>")
	})

	log.Printf("sinflake-server listening on %s (mode=%s)", *addr, *mode)
	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

// writeErr 对齐 LeafController：异常时抛 LeafServerException → HTTP 500，正文是 Result 文本。
// 调用方若只想要机器可读的负数码，加 ?raw=1。
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	if r.URL.Query().Get("raw") == "1" {
		if code := sinflake.ErrorCode(err); code != 0 {
			fmt.Fprint(w, code)
			return
		}
		if code := sinflake.ClockErrorCode(err); code != 0 {
			fmt.Fprint(w, code)
			return
		}
	}
	res := sinflake.OfID(0, err)
	http.Error(w, res.String(), http.StatusInternalServerError)
}

// noKey 对齐 Leaf 的 NoKeyException → 400
func noKey(w http.ResponseWriter) {
	http.Error(w, "no key", http.StatusBadRequest)
}
