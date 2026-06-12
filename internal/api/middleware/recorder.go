package middleware

import "net/http"

type ResponseRecorder struct {
	http.ResponseWriter
	status      int
	bytes       int
	wroteHeader bool
}

func NewResponseRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, status: http.StatusOK}
}

func (r *ResponseRecorder) Status() int { return r.status }

func (r *ResponseRecorder) Bytes() int { return r.bytes }

func (r *ResponseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *ResponseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.status = http.StatusOK
		r.wroteHeader = true
	}
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

func (r *ResponseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *ResponseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
