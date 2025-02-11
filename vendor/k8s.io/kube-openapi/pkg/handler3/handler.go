/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handler3

import (
	"bytes"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang/protobuf/proto"
	openapi_v3 "github.com/google/gnostic/openapiv3"
	"github.com/munnerz/goautoneg"
	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/internal/handler"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"
)

const (
	jsonExt = ".json"

	mimeJson = "application/json"
	// TODO(mehdy): change @68f4ded to a version tag when gnostic add version tags.
	mimePb   = "application/com.github.googleapis.gnostic.OpenAPIv3@68f4ded+protobuf"
	mimePbGz = "application/x-gzip"

	subTypeProtobuf = "com.github.proto-openapi.spec.v3@v1.0+protobuf"
	subTypeJSON     = "json"
)

// OpenAPIService is the service responsible for serving OpenAPI spec. It has
// the ability to safely change the spec while serving it.
// OpenAPI V3 currently does not use the lazy marshaling strategy that OpenAPI V2 is using
type OpenAPIService struct {
	// rwMutex protects All members of this service.
	rwMutex      sync.RWMutex
	lastModified time.Time
	v3Schema     map[string]*OpenAPIV3Group
}

type OpenAPIV3Group struct {
	rwMutex sync.RWMutex

	lastModified time.Time

	pbCache   handler.HandlerCache
	jsonCache handler.HandlerCache
}

func init() {
	mime.AddExtensionType(".json", mimeJson)
	mime.AddExtensionType(".pb-v1", mimePb)
	mime.AddExtensionType(".gz", mimePbGz)
}

func computeETag(data []byte) string {
	return fmt.Sprintf("\"%X\"", sha512.Sum512(data))
}

// NewOpenAPIService builds an OpenAPIService starting with the given spec.
func NewOpenAPIService(spec *spec.Swagger) (*OpenAPIService, error) {
	o := &OpenAPIService{}
	o.v3Schema = make(map[string]*OpenAPIV3Group)
	return o, nil
}

func (o *OpenAPIService) getGroupBytes() ([]byte, error) {
	o.rwMutex.RLock()
	defer o.rwMutex.RUnlock()
	keys := make([]string, len(o.v3Schema))
	i := 0
	for k := range o.v3Schema {
		keys[i] = k
		i++
	}

	sort.Strings(keys)
	group := make(map[string][]string)
	group["Paths"] = keys

	j, err := json.Marshal(group)
	if err != nil {
		return nil, err
	}
	return j, nil
}

func (o *OpenAPIService) getSingleGroupBytes(getType string, group string) ([]byte, string, time.Time, error) {
	o.rwMutex.RLock()
	defer o.rwMutex.RUnlock()
	v, ok := o.v3Schema[group]
	if !ok {
		return nil, "", time.Now(), fmt.Errorf("Cannot find CRD group %s", group)
	}
	if getType == subTypeJSON {
		specBytes, etag, err := v.jsonCache.Get()
		return specBytes, etag, v.lastModified, err
	} else if getType == subTypeProtobuf {
		specPb, etag, err := v.pbCache.Get()
		return specPb, etag, v.lastModified, err
	}
	return nil, "", time.Now(), fmt.Errorf("Invalid accept clause %s", getType)
}

func (o *OpenAPIService) UpdateGroupVersion(group string, openapi *spec3.OpenAPI) (err error) {
	o.rwMutex.Lock()
	defer o.rwMutex.Unlock()

	specBytes, err := json.Marshal(openapi)
	if err != nil {
		return err
	}

	if _, ok := o.v3Schema[group]; !ok {
		o.v3Schema[group] = &OpenAPIV3Group{}
	}
	return o.v3Schema[group].UpdateSpec(specBytes)
}

func (o *OpenAPIService) DeleteGroupVersion(group string) {
	o.rwMutex.Lock()
	defer o.rwMutex.Unlock()
	delete(o.v3Schema, group)
}

func ToV3ProtoBinary(json []byte) ([]byte, error) {
	document, err := openapi_v3.ParseDocument(json)
	if err != nil {
		return nil, err
	}
	return proto.Marshal(document)
}

func (o *OpenAPIService) HandleDiscovery(w http.ResponseWriter, r *http.Request) {
	data, _ := o.getGroupBytes()
	http.ServeContent(w, r, "/openapi/v3", time.Now(), bytes.NewReader(data))
}

func (o *OpenAPIService) HandleGroupVersion(w http.ResponseWriter, r *http.Request) {
	url := strings.SplitAfterN(r.URL.Path, "/", 4)
	group := url[3]

	decipherableFormats := r.Header.Get("Accept")
	if decipherableFormats == "" {
		decipherableFormats = "*/*"
	}
	clauses := goautoneg.ParseAccept(decipherableFormats)
	w.Header().Add("Vary", "Accept")

	if len(clauses) == 0 {
		return
	}

	accepted := []struct {
		Type    string
		SubType string
	}{
		{"application", subTypeJSON},
		{"application", subTypeProtobuf},
	}

	for _, clause := range clauses {
		for _, accepts := range accepted {
			if clause.Type != accepts.Type && clause.Type != "*" {
				continue
			}
			if clause.SubType != accepts.SubType && clause.SubType != "*" {
				continue
			}
			data, etag, lastModified, err := o.getSingleGroupBytes(accepts.SubType, group)
			if err != nil {
				return
			}
			w.Header().Set("Etag", etag)
			http.ServeContent(w, r, "", lastModified, bytes.NewReader(data))
			return
		}
	}
	w.WriteHeader(406)
	return
}

func (o *OpenAPIService) RegisterOpenAPIV3VersionedService(servePath string, handler common.PathHandlerByGroupVersion) error {
	handler.Handle(servePath, http.HandlerFunc(o.HandleDiscovery))
	handler.HandlePrefix(servePath+"/", http.HandlerFunc(o.HandleGroupVersion))
	return nil
}

func (o *OpenAPIV3Group) UpdateSpec(specBytes []byte) (err error) {
	o.rwMutex.Lock()
	defer o.rwMutex.Unlock()

	o.pbCache = o.pbCache.New(func() ([]byte, error) {
		return ToV3ProtoBinary(specBytes)
	})

	o.jsonCache = o.jsonCache.New(func() ([]byte, error) {
		return specBytes, nil
	})
	o.lastModified = time.Now()
	return nil
}
