// Copyright 2023 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package genai is a client for the generative VertexAI model.
package genai

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"cloud.google.com/go/civil"
	aiplatform "cloud.google.com/go/vertexai/internal/aiplatform/apiv1beta1"
	pb "cloud.google.com/go/vertexai/internal/aiplatform/apiv1beta1/aiplatformpb"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	date "google.golang.org/genproto/googleapis/type/date"
)

// A Client is a Google Vertex AI client.
type Client struct {
	c         *aiplatform.PredictionClient
	projectID string
	location  string
}

// NewClient creates a new Google Vertex AI client.
//
// Clients should be reused instead of created as needed. The methods of Client
// are safe for concurrent use by multiple goroutines.
//
// You may configure the client by passing in options from the [google.golang.org/api/option]
// package.
func NewClient(ctx context.Context, projectID, location string, opts ...option.ClientOption) (*Client, error) {
	apiEndpoint := fmt.Sprintf("%s-aiplatform.googleapis.com:443", location)
	c, err := aiplatform.NewPredictionClient(ctx, option.WithEndpoint(apiEndpoint))
	if err != nil {
		return nil, err
	}
	return &Client{
		c:         c,
		projectID: projectID,
		location:  location,
	}, nil
}

// Close closes the client.
func (c *Client) Close() error {
	return c.c.Close()
}

// GenerativeModel is a model that can generate text.
// Create one with [Client.GenerativeModel], then configure
// it by setting the exported fields.
//
// The model holds all the config for a GenerateContentRequest, so the GenerateContent method
// can use a vararg for the content.
type GenerativeModel struct {
	c        *Client
	name     string
	fullName string

	GenerationConfig
	SafetySettings []*SafetySetting
}

const defaultMaxOutputTokens = 2048

// GenerativeModel creates a new instance of the named model.
func (c *Client) GenerativeModel(name string) *GenerativeModel {
	return &GenerativeModel{
		GenerationConfig: GenerationConfig{
			MaxOutputTokens: defaultMaxOutputTokens,
			TopK:            3,
		},
		c:        c,
		name:     name,
		fullName: fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s", c.projectID, c.location, name),
	}
}

// Name returns the name of the model.
func (m *GenerativeModel) Name() string {
	return m.name
}

// GenerateContent produces a single request and response.
func (m *GenerativeModel) GenerateContent(ctx context.Context, parts ...Part) (*GenerateContentResponse, error) {
	return m.generateContent(ctx, m.newGenerateContentRequest(newUserContent(parts)))
}

// GenerateContentStream returns an iterator that enumerates responses.
func (m *GenerativeModel) GenerateContentStream(ctx context.Context, parts ...Part) *GenerateContentResponseIterator {
	streamClient, err := m.c.c.StreamGenerateContent(ctx, m.newGenerateContentRequest(newUserContent(parts)))
	return &GenerateContentResponseIterator{
		sc:  streamClient,
		err: err,
	}
}

func (m *GenerativeModel) generateContent(ctx context.Context, req *pb.GenerateContentRequest) (*GenerateContentResponse, error) {
	streamClient, err := m.c.c.StreamGenerateContent(ctx, req)
	iter := &GenerateContentResponseIterator{
		sc:  streamClient,
		err: err,
	}
	for {
		_, err := iter.Next()
		if err == iterator.Done {
			return iter.merged, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (m *GenerativeModel) newGenerateContentRequest(contents ...*Content) *pb.GenerateContentRequest {
	return &pb.GenerateContentRequest{
		Model:            m.fullName,
		Contents:         mapSlice(contents, (*Content).toProto),
		SafetySettings:   mapSlice(m.SafetySettings, (*SafetySetting).toProto),
		GenerationConfig: m.GenerationConfig.toProto(),
	}
}

func newUserContent(parts []Part) *Content {
	return &Content{Role: roleUser, Parts: parts}
}

// GenerateContentResponseIterator is an iterator over GnerateContentResponse.
type GenerateContentResponseIterator struct {
	sc     pb.PredictionService_StreamGenerateContentClient
	err    error
	merged *GenerateContentResponse
	cs     *ChatSession
}

// Next returns the next response.
func (iter *GenerateContentResponseIterator) Next() (*GenerateContentResponse, error) {
	if iter.err != nil {
		return nil, iter.err
	}
	resp, err := iter.sc.Recv()
	iter.err = err
	if err == io.EOF {
		if iter.cs != nil && iter.merged != nil {
			iter.cs.addToHistory(iter.merged.Candidates)
		}
		return nil, iterator.Done
	}
	if err != nil {
		return nil, err
	}
	gcp, err := protoToResponse(resp)
	if err != nil {
		iter.err = err
		return nil, err
	}
	// Merge this response in with the ones we've already seen.
	iter.merged = joinResponses(iter.merged, gcp)
	// If this is part of a ChatSession, remember the response for the history.
	return gcp, nil
}

// GenerateContentResponse is the response from a GenerateContent or GenerateContentStream call.
type GenerateContentResponse struct {
	Candidates     []*Candidate
	PromptFeedback *PromptFeedback
}

func protoToResponse(resp *pb.GenerateContentResponse) (*GenerateContentResponse, error) {
	// Assume a non-nil PromptFeedback is an error.
	// TODO: confirm.
	pf := (PromptFeedback{}).fromProto(resp.PromptFeedback)
	if pf != nil {
		return nil, &BlockedError{PromptFeedback: pf}
	}
	cands := mapSlice(resp.Candidates, (Candidate{}).fromProto)
	// If any candidate is blocked, error.
	// TODO: is this too harsh?
	for _, c := range cands {
		if c.FinishReason == FinishReasonSafety {
			return nil, &BlockedError{Candidate: c}
		}
	}
	return &GenerateContentResponse{Candidates: cands}, nil
}

// CountTokens counts the number of tokens in the content.
func (m *GenerativeModel) CountTokens(ctx context.Context, parts ...Part) (*CountTokensResponse, error) {
	req := m.newCountTokensRequest(newUserContent(parts))
	res, err := m.c.c.CountTokens(ctx, req)
	if err != nil {
		return nil, err
	}
	return (CountTokensResponse{}).fromProto(res), nil
}

func (m *GenerativeModel) newCountTokensRequest(contents ...*Content) *pb.CountTokensRequest {
	return &pb.CountTokensRequest{
		Endpoint: m.fullName,
		Model:    m.fullName,
		Contents: mapSlice(contents, (*Content).toProto),
	}
}

// A BlockedError indicates that the model's response was blocked.
// There can be two underlying causes: the prompt or a candidate response.
type BlockedError struct {
	// If non-nil, the model's response was blocked.
	// Consult the Candidate and SafetyRatings fields for details.
	Candidate *Candidate

	// If non-nil, there was a problem with the prompt.
	PromptFeedback *PromptFeedback
}

func (e *BlockedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "blocked: ")
	if e.Candidate != nil {
		fmt.Fprintf(&b, "candidate: %s", e.Candidate.FinishReason)
	}
	if e.PromptFeedback != nil {
		if e.Candidate != nil {
			fmt.Fprintf(&b, ", ")
		}
		fmt.Fprintf(&b, "prompt: %v (%s)", e.PromptFeedback.BlockReason, e.PromptFeedback.BlockReasonMessage)
	}
	return b.String()
}

// joinResponses  merges the two responses, which should be the result of a streaming call.
// The first argument is modified.
func joinResponses(dest, src *GenerateContentResponse) *GenerateContentResponse {
	if dest == nil {
		return src
	}
	dest.Candidates = joinCandidateLists(dest.Candidates, src.Candidates)
	// Keep dest.PromptFeedback.
	// TODO: Take the last UsageMetadata.
	return dest
}

func joinCandidateLists(dest, src []*Candidate) []*Candidate {
	indexToSrcCandidate := map[int32]*Candidate{}
	for _, s := range src {
		indexToSrcCandidate[s.Index] = s
	}
	for _, d := range dest {
		s := indexToSrcCandidate[d.Index]
		if s != nil {
			d.Content = joinContent(d.Content, s.Content)
			// Take the last of these.
			d.FinishReason = s.FinishReason
			// d.FinishMessage = s.FinishMessage
			d.SafetyRatings = s.SafetyRatings
			d.CitationMetadata = joinCitationMetadata(d.CitationMetadata, s.CitationMetadata)
		}
	}
	return dest
}

func joinCitationMetadata(dest, src *CitationMetadata) *CitationMetadata {
	if dest == nil {
		return src
	}
	if src == nil {
		return dest
	}
	dest.Citations = append(dest.Citations, src.Citations...)
	return dest
}

func joinContent(dest, src *Content) *Content {
	if dest == nil {
		return src
	}
	// Assume roles are the same.
	dest.Parts = joinParts(dest.Parts, src.Parts)
	return dest
}

func joinParts(dest, src []Part) []Part {
	return mergeTexts(append(dest, src...))
}

func mergeTexts(in []Part) []Part {
	var out []Part
	i := 0
	for i < len(in) {
		if t, ok := in[i].(Text); ok {
			texts := []string{string(t)}
			var j int
			for j = i + 1; j < len(in); j++ {
				if t, ok := in[j].(Text); ok {
					texts = append(texts, string(t))
				} else {
					break
				}
			}
			// j is just after the last Text.
			out = append(out, Text(strings.Join(texts, "")))
			i = j
		} else {
			out = append(out, in[i])
			i++
		}
	}
	return out
}

func civilDateToProto(d civil.Date) *date.Date {
	return &date.Date{
		Year:  int32(d.Year),
		Month: int32(d.Month),
		Day:   int32(d.Day),
	}
}

func civilDateFromProto(p *date.Date) civil.Date {
	if p == nil {
		return civil.Date{}
	}
	return civil.Date{
		Year:  int(p.Year),
		Month: time.Month(p.Month),
		Day:   int(p.Day),
	}
}
