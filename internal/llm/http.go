package llm

import (
	"crypto/tls"
	"net/http"
)

type requestAlias = http.Request
type responseAlias = http.Response

func httpClient(config Config) HTTPDoer {
	if config.HTTP != nil {
		return config.HTTP
	}
	if config.InsecureSkipVerify {
		return &http.Client{
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: true,
				},
			},
		}
	}
	return http.DefaultClient
}
