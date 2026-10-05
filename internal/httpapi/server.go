// Package httpapi 暴露元数据目录的本地 HTTP 接口，全部使用
// Go 标准库 net/http，便于手工 curl 验证。
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/lechandonga/metadata-lineage-catalog-go/internal/catalog"
)

type Server struct {
	cat *catalog.Catalog
	mux *http.ServeMux
}

func New(cat *catalog.Catalog) *Server {
	s := &Server{cat: cat, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	s.mux.HandleFunc("/healthz", s.health)
	s.mux.HandleFunc("/assets", s.assets)
	s.mux.HandleFunc("/assets/", s.assetSub)
	s.mux.HandleFunc("/edges", s.edges)
	s.mux.HandleFunc("/impact", s.impact)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// POST /assets                         登记资产
// GET  /assets[?include_deleted=true]  列出资产
func (s *Server) assets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in catalog.RegisterAssetInput
		if !decode(w, r, &in) {
			return
		}
		a, created, err := s.cat.RegisterAsset(in)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"asset": a, "created": created})
	case http.MethodGet:
		includeDeleted := r.URL.Query().Get("include_deleted") == "true"
		writeJSON(w, http.StatusOK, map[string]any{"assets": s.cat.ListAssets(includeDeleted)})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET/POST"))
	}
}

// /assets/{key}                GET 查资产 | DELETE 软删除
// /assets/{key}/versions       GET 版本列表 | POST 登记下一版
// /assets/{key}/versions/{v}   GET 指定版本
// /assets/{key}/diff?from=&to= GET 版本差异
func (s *Server) assetSub(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/assets/")
	parts := strings.Split(path, "/")
	key := parts[0]
	if key == "" {
		writeJSON(w, http.StatusNotFound, errorBody("not_found", "缺少资产键"))
		return
	}
	switch {
	case len(parts) == 1:
		s.assetByKey(w, r, key)
	case len(parts) == 2 && parts[1] == "versions":
		s.versions(w, r, key)
	case len(parts) == 3 && parts[1] == "versions":
		s.version(w, r, key, parts[2])
	case len(parts) == 2 && parts[1] == "diff":
		s.diff(w, r, key)
	default:
		writeJSON(w, http.StatusNotFound, errorBody("not_found", "未知路径: "+r.URL.Path))
	}
}

func (s *Server) assetByKey(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		a, err := s.cat.GetAsset(key)
		if err != nil {
			writeError(w, err)
			return
		}
		versions, _ := s.cat.Versions(key)
		writeJSON(w, http.StatusOK, map[string]any{"asset": a, "versions": versions})
	case http.MethodDelete:
		if err := s.cat.DeleteAsset(key); err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "key": key})
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET/DELETE"))
	}
}

type putVersionReq struct {
	Columns []catalog.Column `json:"columns"`
}

func (s *Server) versions(w http.ResponseWriter, r *http.Request, key string) {
	switch r.Method {
	case http.MethodGet:
		vs, err := s.cat.Versions(key)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"versions": vs})
	case http.MethodPost:
		var req putVersionReq
		if !decode(w, r, &req) {
			return
		}
		v, created, err := s.cat.PutVersion(key, req.Columns)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"version": v, "created": created})
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET/POST"))
	}
}

func (s *Server) version(w http.ResponseWriter, r *http.Request, key, vText string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET"))
		return
	}
	v, err := strconv.Atoi(vText)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("validation_error", "版本号必须是整数"))
		return
	}
	ver, err := s.cat.GetVersion(key, v)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version": ver})
}

func (s *Server) diff(w http.ResponseWriter, r *http.Request, key string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET"))
		return
	}
	q := r.URL.Query()
	from, err1 := strconv.Atoi(q.Get("from"))
	to, err2 := strconv.Atoi(q.Get("to"))
	if err1 != nil || err2 != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("validation_error", "from/to 必须是版本号整数"))
		return
	}
	d, err := s.cat.DiffVersions(key, from, to)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// POST   /edges       登记血缘边
// DELETE /edges       撤销血缘边（body 或 query 指定端点）
// GET    /edges       列出全部边
func (s *Server) edges(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var in catalog.EdgeInput
		if !decode(w, r, &in) {
			return
		}
		e, created, err := s.cat.PutEdge(in)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusOK
		if created {
			status = http.StatusCreated
		}
		writeJSON(w, status, map[string]any{"edge": e, "created": created})
	case http.MethodDelete:
		var in catalog.EdgeInput
		if r.Body != nil && r.ContentLength != 0 {
			if !decode(w, r, &in) {
				return
			}
		} else {
			q := r.URL.Query()
			in = catalog.EdgeInput{
				SrcAssetKey: q.Get("src_asset_key"),
				SrcColumn:   q.Get("src_column"),
				DstAssetKey: q.Get("dst_asset_key"),
				DstColumn:   q.Get("dst_column"),
			}
		}
		deleted, err := s.cat.DeleteEdge(in)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"edges": s.cat.ListEdges()})
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET/POST/DELETE"))
	}
}

// GET /impact?asset_key=&column=  查询字段下游影响面
func (s *Server) impact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, errorBody("method_not_allowed", "仅支持 GET"))
		return
	}
	q := r.URL.Query()
	res, err := s.cat.Impact(q.Get("asset_key"), q.Get("column"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("validation_error", "请求体不是合法 JSON: "+err.Error()))
		return false
	}
	return true
}

func errorBody(kind, reason string) map[string]any {
	return map[string]any{"error": map[string]string{"kind": kind, "reason": reason}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, err error) {
	var ce *catalog.CatalogError
	if !errors.As(err, &ce) {
		writeJSON(w, http.StatusInternalServerError, errorBody("internal_error", err.Error()))
		return
	}
	body := map[string]any{"error": map[string]any{
		"kind":   string(ce.Kind),
		"reason": ce.Reason,
	}}
	if len(ce.Details) > 0 {
		body["error"].(map[string]any)["details"] = ce.Details
	}
	if len(ce.Changes) > 0 {
		body["error"].(map[string]any)["incompatible_changes"] = ce.Changes
	}
	writeJSON(w, statusFor(ce.Kind), body)
}

func statusFor(k catalog.ErrorKind) int {
	switch k {
	case catalog.ErrValidation:
		return http.StatusBadRequest
	case catalog.ErrNotFound:
		return http.StatusNotFound
	case catalog.ErrAmbiguousKey:
		return http.StatusConflict
	case catalog.ErrDanglingReference:
		return http.StatusUnprocessableEntity
	case catalog.ErrConflict, catalog.ErrIncompatibleSchema:
		return http.StatusUnprocessableEntity
	case catalog.ErrCorruptStore, catalog.ErrUnsupportedVersion:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}
