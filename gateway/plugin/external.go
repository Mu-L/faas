// License: OpenFaaS Community Edition (CE) EULA
// Copyright (c) 2017,2019-2024 OpenFaaS Author(s)

// Copyright (c) Alex Ellis 2017. All rights reserved.

package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	types "github.com/openfaas/faas-provider/types"
	middleware "github.com/openfaas/faas/gateway/pkg/middleware"
	"github.com/openfaas/faas/gateway/scaling"
)

// ExternalServiceQuery proxies service queries to external plugin via HTTP
type ExternalServiceQuery struct {
	URL          url.URL
	ProxyClient  http.Client
	AuthInjector middleware.AuthInjector

	// IncludeUsage includes usage metrics in the response
	IncludeUsage bool
}

// NewExternalServiceQuery proxies service queries to external plugin via HTTP
func NewExternalServiceQuery(externalURL url.URL, authInjector middleware.AuthInjector) scaling.ServiceQuery {
	timeout := 3 * time.Second

	proxyClient := http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   timeout,
				KeepAlive: 0,
			}).DialContext,
			MaxIdleConns:          1,
			DisableKeepAlives:     true,
			IdleConnTimeout:       120 * time.Millisecond,
			ExpectContinueTimeout: 1500 * time.Millisecond,
		},
	}

	return ExternalServiceQuery{
		URL:          externalURL,
		ProxyClient:  proxyClient,
		AuthInjector: authInjector,
		IncludeUsage: false,
	}
}

// GetReplicas replica count for function
func (s ExternalServiceQuery) GetReplicas(serviceName, serviceNamespace string) (scaling.ServiceQueryResponse, error) {
	start := time.Now()

	var err error
	var emptyServiceQueryResponse scaling.ServiceQueryResponse

	function := types.FunctionStatus{}

	urlPath := fmt.Sprintf("%ssystem/function/%s?namespace=%s&usage=%v",
		s.URL.String(),
		serviceName,
		serviceNamespace,
		s.IncludeUsage)

	req, err := http.NewRequest(http.MethodGet, urlPath, nil)
	if err != nil {
		return emptyServiceQueryResponse, err
	}

	if s.AuthInjector != nil {
		s.AuthInjector.Inject(req)
	}

	res, err := s.ProxyClient.Do(req)
	if err != nil {
		log.Println(urlPath, err)
		return emptyServiceQueryResponse, err

	}

	var bytesOut []byte
	if res.Body != nil {
		bytesOut, _ = io.ReadAll(res.Body)
		defer res.Body.Close()
	}

	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(bytesOut, &function); err != nil {
			log.Printf("Unable to unmarshal: %q, %s", string(bytesOut), err)
			return emptyServiceQueryResponse, err
		}

		// log.Printf("GetReplicas [%s.%s] took: %fs", serviceName, serviceNamespace, time.Since(start).Seconds())

	} else {
		log.Printf("GetReplicas [%s.%s] took: %.4fs, code: %d\n", serviceName, serviceNamespace, time.Since(start).Seconds(), res.StatusCode)
		return emptyServiceQueryResponse, fmt.Errorf("server returned non-200 status code (%d) for function, %s, body: %s", res.StatusCode, serviceName, string(bytesOut))
	}

	minReplicas := uint64(scaling.DefaultMinReplicas)
	maxReplicas := uint64(scaling.DefaultMaxReplicas)
	scalingFactor := uint64(scaling.DefaultScalingFactor)
	availableReplicas := function.AvailableReplicas

	if function.Labels != nil {
		labels := *function.Labels

		minReplicas = extractLabelValue(labels[scaling.MinScaleLabel], minReplicas)
		maxReplicas = extractLabelValue(labels[scaling.MaxScaleLabel], maxReplicas)
		extractedScalingFactor := extractLabelValue(labels[scaling.ScalingFactorLabel], scalingFactor)

		if extractedScalingFactor > 0 && extractedScalingFactor <= 100 {
			scalingFactor = extractedScalingFactor
		} else {
			return scaling.ServiceQueryResponse{}, fmt.Errorf("bad scaling factor: %d, is not in range of [0 - 100]", extractedScalingFactor)
		}
	}

	return scaling.ServiceQueryResponse{
		Replicas:          function.Replicas,
		MaxReplicas:       maxReplicas,
		MinReplicas:       minReplicas,
		ScalingFactor:     scalingFactor,
		AvailableReplicas: availableReplicas,
		Annotations:       function.Annotations,
	}, err
}

// SetReplicas update the replica count
func (s ExternalServiceQuery) SetReplicas(serviceName, serviceNamespace string, count uint64) error {
	var err error

	scaleReq := types.ScaleServiceRequest{
		ServiceName: serviceName,
		Replicas:    count,
	}

	requestBody, err := json.Marshal(scaleReq)
	if err != nil {
		return err
	}

	start := time.Now()
	urlPath := fmt.Sprintf("%ssystem/scale-function/%s?namespace=%s", s.URL.String(), serviceName, serviceNamespace)
	req, _ := http.NewRequest(http.MethodPost, urlPath, bytes.NewReader(requestBody))

	if s.AuthInjector != nil {
		s.AuthInjector.Inject(req)
	}

	defer req.Body.Close()
	res, err := s.ProxyClient.Do(req)

	if err != nil {
		log.Println(urlPath, err)
	} else {
		if res.Body != nil {
			defer res.Body.Close()
		}
	}

	if !(res.StatusCode == http.StatusOK || res.StatusCode == http.StatusAccepted) {
		err = fmt.Errorf("error scaling HTTP code %d, %s", res.StatusCode, urlPath)
	}

	log.Printf("SetReplicas [%s.%s] took: %.4fs",
		serviceName, serviceNamespace, time.Since(start).Seconds())

	return err
}

// extractLabelValue will parse the provided raw label value and if it fails
// it will return the provided fallback value and log an message
func extractLabelValue(rawLabelValue string, fallback uint64) uint64 {
	if len(rawLabelValue) <= 0 {
		return fallback
	}

	value, err := strconv.Atoi(rawLabelValue)

	if err != nil {
		log.Printf("Provided label value %s should be of type uint", rawLabelValue)
		return fallback
	}

	return uint64(value)
}
