package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// Server 把 Catalog 暴露为仅依赖标准库的 HTTP 服务。
type Server struct {
	catalog *Catalog
	logger  *log.Logger
	mux     *http.ServeMux
}

// NewServer 构造 HTTP 服务。
func NewServer(c *Catalog, logger *log.Logger) *Server {
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{catalog: c, logger: logger, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 返回路由处理器。
func (s *Server) Handler() http.Handler { return s.mux }

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.handleHealth)
	s.mux.HandleFunc("/api/v1/assets", s.handleAssets)
	s.mux.HandleFunc("/api/v1/assets/", s.handleAssetSub)
	s.mux.HandleFunc("/api/v1/edges", s.handleEdges)
	s.mux.HandleFunc("/api/v1/impact", s.handleImpact)
}

type registerAssetRequest struct {
	Namespace string   `json:"namespace"`
	Asset     string   `json:"asset"`
	Version   string   `json:"version"`
	Columns   []Column `json:"columns"`
}

type edgeRequest struct {
	Source FieldRef `json:"source"`
	Target FieldRef `json:"target"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		includeDeleted := r.URL.Query().Get("include_deleted") == "true"
		writeJSON(w, http.StatusOK, map[string]any{"assets": s.catalog.ListAssets(includeDeleted)})
	case http.MethodPost:
		var req registerAssetRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
		res, err := s.catalog.RegisterAsset(req.Namespace, req.Asset, req.Version, req.Columns)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeError(w, errf(KindValidation, "方法 %s 不被允许", r.Method))
	}
}

// handleAssetSub 处理:
//   - /api/v1/assets/{namespace}/{asset}              GET 资产 / DELETE 删除
//   - /api/v1/assets/{namespace}/{asset}/versions/{v} GET 指定版本
//   - /api/v1/assets/{namespace}/{asset}/diff?from=&to= GET 两版本差异
func (s *Server) handleAssetSub(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/assets/")
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 {
		writeError(w, errf(KindValidation, "路径必须为 /api/v1/assets/{namespace}/{asset}[...]"))
		return
	}
	ref := AssetRef{Namespace: parts[0], Asset: parts[1]}

	switch {
	case len(parts) == 2:
		switch r.Method {
		case http.MethodGet:
			a, err := s.catalog.GetAsset(ref)
			if err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, a)
		case http.MethodDelete:
			if err := s.catalog.DeleteAsset(ref.Namespace, ref.Asset); err != nil {
				writeError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "deleted", "asset_key": ref.Namespace + "/" + ref.Asset})
		default:
			w.Header().Set("Allow", "GET, DELETE")
			writeError(w, errf(KindValidation, "方法 %s 不被允许", r.Method))
		}
	case len(parts) == 4 && parts[2] == "versions":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, errf(KindValidation, "方法 %s 不被允许", r.Method))
			return
		}
		v, err := s.catalog.GetVersion(ref, parts[3])
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	case len(parts) == 3 && parts[2] == "diff":
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			writeError(w, errf(KindValidation, "方法 %s 不被允许", r.Method))
			return
		}
		d, err := s.catalog.DiffVersions(ref, r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, d)
	default:
		writeError(w, errf(KindValidation, "不支持的资产子路径: %s", r.URL.Path))
	}
}

func (s *Server) handleEdges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"edges": s.catalog.ListEdges()})
	case http.MethodPost:
		var req edgeRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
		if err := s.catalog.AddEdge(req.Source, req.Target); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "registered", "edge": req.Source.String() + " -> " + req.Target.String()})
	case http.MethodDelete:
		var req edgeRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, err)
			return
		}
		res, err := s.catalog.RemoveEdge(req.Source, req.Target)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeError(w, errf(KindValidation, "方法 %s 不被允许", r.Method))
	}
}

func (s *Server) handleImpact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeError(w, errf(KindValidation, "方法 %s 不被允许", r.Method))
		return
	}
	ref, cerr := ParseFieldRef(r.URL.Query().Get("field"))
	if cerr != nil {
		writeError(w, cerr)
		return
	}
	res, err := s.catalog.Impact(ref)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return errf(KindValidation, "请求体不是合法 JSON: %v", err)
	}
	if dec.More() {
		return errf(KindValidation, "请求体包含多余内容")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(body); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

// statusForKind 把领域错误类型映射为 HTTP 状态码。
func statusForKind(k ErrorKind) int {
	switch k {
	case KindValidation:
		return http.StatusBadRequest
	case KindNotFound, KindDangling:
		return http.StatusNotFound
	case KindDeleted:
		return http.StatusGone
	case KindAmbiguous:
		return http.StatusConflict
	case KindConflict, KindCycle, KindIncompatibleSchema:
		return http.StatusUnprocessableEntity
	case KindCorrupt:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func writeError(w http.ResponseWriter, err error) {
	var ce *Error
	if !errors.As(err, &ce) {
		ce = errf(KindCorrupt, "内部错误: %v", err)
	}
	status := statusForKind(ce.Kind)
	writeJSON(w, status, map[string]any{
		"error":   ce.Kind,
		"message": ce.Message,
		"details": ce.Details,
	})
	if status >= 500 {
		log.Printf("请求失败: %v", err)
	}
}

// ListenAndServe 在 addr 上启动服务(阻塞)。
func (s *Server) ListenAndServe(addr string) error {
	s.logger.Printf("元数据目录服务已启动,监听 %s", addr)
	srv := &http.Server{Addr: addr, Handler: s.mux}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP 服务退出: %w", err)
	}
	return nil
}
