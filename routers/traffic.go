package routers

import (
	"net/http"
)

func recordTraffic() func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(resp http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(resp, req)

		})
	}
}
