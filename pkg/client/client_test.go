package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/doordash-oss/oapi-codegen-dd/v3/pkg/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/msgvault/internal/contentverify"
	"go.kenn.io/msgvault/pkg/client/generated"
)

func TestGeneratedSavedViewStateRoundTripsCanonicalDefinition(t *testing.T) {
	want := `{
		"query":"invoice",
		"search_mode":"full_text",
		"filters":[{"field":"source_id","operator":"eq","values":["9007199254740993"]}],
		"grouping":["sender"],
		"presentation":"table",
		"sort":[{"field":"sent_at","direction":"desc"}],
		"columns":["sender","subject"],
		"inspector_pinned":true
	}`

	var state generated.SavedViewStateEnvelope
	require.NoError(t, json.Unmarshal([]byte(want), &state))
	got, err := json.Marshal(state)
	require.NoError(t, err)
	assert.JSONEq(t, want, string(got))
}

func TestGeneratedEnumNamesPreserveSavedViewCompatibilityAndQualifyExploration(t *testing.T) {
	assertions := assert.New(t)
	assertions.Equal(generated.Asc, generated.SavedViewSortDirection("asc"))
	assertions.Equal(generated.Desc, generated.SavedViewSortDirection("desc"))
	assertions.Equal(generated.IdentitySearchSortDirectionAsc, generated.IdentitySearchSortDirection("asc"))
	assertions.Equal(generated.IdentitySearchSortDirectionDesc, generated.IdentitySearchSortDirection("desc"))
	assertions.Equal(generated.Files, generated.SavedViewStateEnvelopePresentation("files"))
	assertions.Equal(generated.Table, generated.SavedViewStateEnvelopePresentation("table"))
	assertions.Equal(generated.Timeline, generated.SavedViewStateEnvelopePresentation("timeline"))
	assertions.Equal(generated.ExploreFilterDimensionAfter, generated.ExploreFilterDimension("after"))
	assertions.Equal(generated.ExploreGroupSortDirectionAsc, generated.ExploreGroupSortDirection("asc"))
	assertions.Equal(generated.ExploreGroupDimensionSource, generated.ExploreGroupDimension("source"))
	assertions.Equal(generated.ExploreGroupsHTTPRequestSearchModeFullText, generated.ExploreGroupsHTTPRequestSearchMode("full_text"))
}

func TestGeneratedExploreGroupingValidatesExactlyOneDimension(t *testing.T) {
	requirements := require.New(t)
	valid := generated.ExploreGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource,
	}}
	requirements.NoError(valid.Validate())

	requirements.Error((generated.ExploreGroupsHTTPRequest{}).Validate(), "empty grouping")
	requirements.Error((generated.ExploreGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource, generated.ExploreGroupDimensionMonth,
	}}).Validate(), "multiple grouping dimensions")

	fileValid := generated.FileGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource,
	}}
	requirements.NoError(fileValid.Validate())
	requirements.Error((generated.FileGroupsHTTPRequest{}).Validate(), "empty file grouping")
	requirements.Error((generated.FileGroupsHTTPRequest{Grouping: []generated.ExploreGroupDimension{
		generated.ExploreGroupDimensionSource, generated.ExploreGroupDimensionMonth,
	}}).Validate(), "multiple file grouping dimensions")
}

func TestGeneratedFileMetadataRequiresPresenceButAcceptsEmptyLegacyStrings(t *testing.T) {
	t.Run("metadata response", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.FileMetadataResponse
		requirements.NoError(json.Unmarshal([]byte(
			`{"content_state":"metadata_only","entry_key":"source:1:message:m1","filename":"","mime_type":""}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})

	t.Run("search row", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var present generated.FileSearchRow
		requirements.NoError(json.Unmarshal([]byte(
			`{"containing_title":"item","content_state":"metadata_only","entry_key":"message:1","filename":"","key":"file:1","mime_family":"other","mime_type":"","occurred_at":"2026-07-19T12:00:00Z","source_identifier":"archive@example.com","source_type":"synthetic"}`,
		), &present))
		requirements.NotNil(present.Filename)
		requirements.NotNil(present.MimeType)
		assertions.Empty(*present.Filename)
		assertions.Empty(*present.MimeType)
		requirements.NoError(present.Validate(), "present empty strings are legitimate legacy metadata")

		missingFilename := present
		missingFilename.Filename = nil
		requirements.Error(missingFilename.Validate(), "missing required filename")
		missingMIME := present
		missingMIME.MimeType = nil
		requirements.Error(missingMIME.Validate(), "missing required MIME type")
	})
}

// TestGeneratedChangesResponseAcceptsTheFeedsOrdinaryPages holds the
// content-change feed to the same "required means present, not non-empty"
// distinction the file-metadata models above are held to.
//
// A row of that feed omits every column it has nothing to say about: a live
// message carries no deletion timestamps, a chat message carries no subject,
// snippet, or platform id, and the first poll of an empty archive has no cursor
// to echo. Declaring any of those required in the OpenAPI document makes this
// validator reject them — required here means non-nil AND non-empty — so the
// published client would refuse the server's ordinary successful responses.
func TestGeneratedChangesResponseAcceptsTheFeedsOrdinaryPages(t *testing.T) {
	const liveEmail = `{
		"messages":[{
			"id":918,
			"source_id":1,
			"source_message_id":"18f2c9d0a1b3",
			"conversation_id":44,
			"message_type":"email",
			"subject":"Q4 planning",
			"snippet":"Here's the draft for Q4...",
			"sent_at":"2026-03-01T10:00:00Z",
			"size_estimate":8412,
			"has_attachments":false,
			"attachment_count":0,
			"content_changed_at":"2026-07-26T10:00:00.731123Z"
		}],
		"count":1,
		"has_more":false,
		"next_since":"2026-07-26T10:00:00.731123Z",
		"next_since_id":918,
		"server_time":"2026-07-26T10:00:03.114500Z",
		"complete_through":"2026-07-26T10:00:03.114488Z"
	}`

	t.Run("live message omits every unset timestamp", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(liveEmail), &page))
		requirements.NoError(page.Validate(),
			"a message that was never deleted and has no platform timestamps is the "+
				"common case, not an error")
		requirements.Len(page.Messages, 1)
		row := page.Messages[0]
		assertions.Nil(row.ReceivedAt, "received_at")
		assertions.Nil(row.InternalDate, "internal_date")
		assertions.Nil(row.DeletedAt, "deleted_at")
		assertions.Nil(row.DeletedFromSourceAt, "deleted_from_source_at")
	})

	t.Run("chat message omits subject, snippet, and platform id", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(`{
			"messages":[{
				"id":7,
				"source_id":2,
				"conversation_id":9,
				"message_type":"imessage",
				"size_estimate":0,
				"has_attachments":false,
				"attachment_count":0,
				"content_changed_at":"2026-07-26T10:00:00.731123Z"
			}],
			"count":1,
			"has_more":false,
			"next_since":"2026-07-26T10:00:00.731123Z",
			"next_since_id":7,
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":"2026-07-26T10:00:03.114488Z"
		}`), &page))
		requirements.NoError(page.Validate(),
			"chat platforms carry no subject and the store COALESCEs a missing "+
				"platform id to the empty string")
		requirements.Len(page.Messages, 1)
		row := page.Messages[0]
		assertions.Nil(row.Subject, "subject")
		assertions.Nil(row.Snippet, "snippet")
		assertions.Nil(row.SourceMessageID, "source_message_id")
	})

	t.Run("empty archive page carries no cursor", func(t *testing.T) {
		assertions := assert.New(t)
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(`{
			"messages":[],
			"count":0,
			"has_more":false,
			"next_since_id":0,
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":"2026-07-26T10:00:03.114488Z"
		}`), &page))
		requirements.NoError(page.Validate(),
			"a first poll of an empty archive has no last row and no request cursor "+
				"to echo, so next_since is absent")
		assertions.Empty(page.Messages, "messages")
		assertions.Nil(page.NextSince, "next_since")
	})

	t.Run("a missing watermark is still rejected", func(t *testing.T) {
		requirements := require.New(t)
		var page generated.ChangesResponse
		requirements.NoError(json.Unmarshal([]byte(liveEmail), &page))
		requirements.Len(page.Messages, 1)
		page.Messages[0].ContentChangedAt = ""
		requirements.Error(page.Validate(),
			"content_changed_at is the cursor: a row without it cannot be resumed "+
				"from, so loosening the other fields must not loosen this one")
		page.Messages[0].ContentChangedAt = "2026-07-26T10:00:00.731123Z"
		page.ServerTime = ""
		requirements.Error(page.Validate(),
			"server_time is always a database clock reading")
		page.ServerTime = "2026-07-26T10:00:03.114500Z"
		page.CompleteThrough = ""
		requirements.Error(page.Validate(),
			"complete_through tells a consumer how far the feed is caught up; without "+
				"it a feed held back by an open write transaction is indistinguishable "+
				"from a caught-up one")
	})
}

// TestListChangedMessagesRoundTripsASubSecondCursor covers the half of the
// change feed's client contract the response-model test above cannot reach:
// that the generated client builds the request the server accepts, and that a
// cursor survives the trip out through the generated query parameters with its
// sub-second precision intact.
//
// The feed's cursor carries the database's full sub-second resolution and the
// consumer is told to send it back verbatim. A cursor truncated to whole
// seconds on the way out sits below the watermark of the page it came from, so
// the consumer is handed that same page on every poll, forever; one rounded the
// other way steps over whatever was stamped in between. Neither shows up in the
// response model — only in the query string. So this walks the loop a consumer
// walks, taking next_since from one page and sending it as the next request,
// and asserts on what the client actually put on the wire.
func TestListChangedMessagesRoundTripsASubSecondCursor(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	const cursor = "2026-07-26T10:00:00.731123Z"
	const cursorID = int64(918)

	var gotMethod, gotPath string
	var gotQuery url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"messages":[{
				"id":%d,
				"source_id":1,
				"conversation_id":44,
				"message_type":"email",
				"subject":"Q4 planning",
				"size_estimate":8412,
				"has_attachments":false,
				"attachment_count":0,
				"content_changed_at":%q
			}],
			"count":1,
			"has_more":false,
			"next_since":%q,
			"next_since_id":%d,
			"server_time":"2026-07-26T10:00:03.114500Z",
			"complete_through":"2026-07-26T10:00:03.114488Z"
		}`, cursorID, cursor, cursor, cursorID)
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	// First poll: a consumer with no cursor yet. Sending since= or since_id=0
	// would be a different request than omitting them, so the client must omit.
	limit := int64(100)
	first, err := c.ListChangedMessages(context.Background(), &generated.ListChangedMessagesRequestOptions{
		Query: &generated.ListChangedMessagesQuery{Limit: &limit},
	})
	require.NoError(err, "ListChangedMessages first poll")
	assert.Equal(http.MethodGet, gotMethod, "method")
	assert.Equal("/api/v1/messages/changes", gotPath, "path")
	assert.Equal("100", gotQuery.Get("limit"), "limit query")
	assert.NotContains(gotQuery, "since", "an absent cursor must not be sent as an empty one")
	assert.NotContains(gotQuery, "since_id", "an absent tiebreak must not be sent as a zero one")

	require.NotNil(first, "first page")
	require.NotNil(first.NextSince, "next_since")
	assert.Equal(cursor, *first.NextSince,
		"the decoded cursor must keep every digit the server published")
	assert.Equal(cursorID, first.NextSinceID, "next_since_id")
	require.Len(first.Messages, 1, "messages")
	assert.Equal(cursor, first.Messages[0].ContentChangedAt, "the row's watermark")

	// Second poll: the response fed straight back, exactly as the docs tell a
	// consumer to do it.
	second, err := c.ListChangedMessages(context.Background(), &generated.ListChangedMessagesRequestOptions{
		Query: &generated.ListChangedMessagesQuery{
			Since:   first.NextSince,
			SinceID: &first.NextSinceID,
			Limit:   &limit,
		},
	})
	require.NoError(err, "ListChangedMessages second poll")
	assert.Equal(cursor, gotQuery.Get("since"),
		"the cursor reached the wire truncated or reformatted: a consumer that "+
			"sends it back no longer resumes where the page ended")
	assert.Equal("918", gotQuery.Get("since_id"), "since_id query")
	assert.Equal("100", gotQuery.Get("limit"), "limit query")
	require.NotNil(second, "second page")
}

func TestGeneratedGetAttachmentContentReturnsBinaryBytes(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte{0x00, 0xff, 0x7b, 0x22, 0x6e, 0x6f, 0x74, 0x2d, 0x6a, 0x73, 0x6f, 0x6e}
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodGet, r.Method, "method")
		assert.Equal("/api/v1/attachments/"+hash+"/content", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(content)
	}))
	t.Cleanup(server.Close)

	c, err := generated.NewDefaultClient(server.URL, runtime.WithHTTPClient(httpClientDoer{client: http.DefaultClient}))
	require.NoError(err, "NewDefaultClient")

	got, err := c.GetAttachmentContent(context.Background(), &generated.GetAttachmentContentRequestOptions{
		PathParams: &generated.GetAttachmentContentPath{Hash: hash},
	})
	require.NoError(err, "GetAttachmentContent")
	require.NotNil(got, "response")
	assert.Equal(content, *got, "content")
}

func TestGetAttachmentContentVerifiesRequestedHash(t *testing.T) {
	require := require.New(t)
	want := []byte("public client attachment")
	corrupt := bytes.Clone(want)
	corrupt[0] ^= 0xff
	hash := fmt.Sprintf("%x", sha256.Sum256(want))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(corrupt)
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err)
	options := &generated.GetAttachmentContentRequestOptions{
		PathParams: &generated.GetAttachmentContentPath{Hash: hash},
	}
	_, err = c.GetAttachmentContent(context.Background(), options)
	require.ErrorIs(err, contentverify.ErrMismatch)
	response, err := c.GetAttachmentContentWithResponse(context.Background(), options)
	require.ErrorIs(err, contentverify.ErrMismatch)
	require.NotNil(response)
	assert.Equal(t, corrupt, response.Body)
}

func TestNewCreatesTypedClient(t *testing.T) {
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/api/v1/stats", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"total_messages":3}`))
	}))
	t.Cleanup(server.Close)

	client, err := New(server.URL)
	require.NoError(
		err, "New")

	stats, err := client.GetStats(context.Background())
	require.NoError(
		err, "GetStats")

	require.NotNil(stats)
	assert.Equal(t, int64(3), stats.TotalMessages)
}

func TestRunQueryDecodesScalarCells(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/query", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"columns":["n","s","b"],"rows":[[1,"x",true]],"row_count":1}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	got, err := c.RunQuery(context.Background(), &generated.RunQueryRequestOptions{
		Body: &generated.RunQueryBody{SQL: "SELECT 1"},
	})
	require.NoError(
		err, "RunQuery")

	assert.Equal([]string{"n", "s", "b"}, got.Columns, "columns")
	require.Len(got.Rows, 1, "rows")
	numberCell, ok := got.Rows[0][0].(float64)
	require.True(ok, "number cell type")
	assert.InDelta(1.0, numberCell, 0, "number cell")
	assert.Equal("x", got.Rows[0][1], "string cell")
	assert.Equal(true, got.Rows[0][2], "bool cell")
}

func TestGetMessageRendersLargeIDInPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages/24489626", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":24489626,"subject":"Large ID"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	resp, err := c.GetMessageWithResponse(context.Background(), &generated.GetMessageRequestOptions{
		PathParams: &generated.GetMessagePath{ID: 24489626},
	})
	require.NoError(err, "GetMessageWithResponse")
	require.NotNil(resp.JSON200, "JSON200")
	assert.Equal(int64(24489626), resp.JSON200.ID, "id")
}

func TestListMessagesRendersLargeQueryValue(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal("/api/v1/messages", r.URL.Path, "path")
		assert.Equal("12345678", r.URL.Query().Get("page"), "page query")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"page":12345678,"page_size":20,"total":0}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(err, "New")

	page := int64(12345678)
	resp, err := c.ListMessagesWithResponse(context.Background(), &generated.ListMessagesRequestOptions{
		Query: &generated.ListMessagesQuery{Page: &page},
	})
	require.NoError(err, "ListMessagesWithResponse")
	require.NotNil(resp.JSON200, "JSON200")
	assert.Equal(int64(12345678), resp.JSON200.Page, "page")
}

func TestAddAccountAcceptsIdempotentOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/accounts", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","message":"account already exists"}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	got, err := c.AddAccount(context.Background(), &generated.AddAccountRequestOptions{
		Body: &generated.AddAccountBody{
			Email:    "alice@example.com",
			Enabled:  true,
			Schedule: "0 2 * * *",
		},
	})
	require.NoError(
		err, "AddAccount")

	assert.Equal("ok", got.Status, "status")
	assert.Equal("account already exists", got.Message, "message")
}

func TestStageDeletionAcceptsDryRunOK(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(http.MethodPost, r.Method, "method")
		assert.Equal("/api/v1/deletions", r.URL.Path, "path")
		w.Header().Set("Content-Type", "application/json")
		// Dry runs return 200, not 201.
		_, _ = w.Write([]byte(`{"dry_run":true,"message_count":3,"sample_gmail_ids":["gm-1","gm-2","gm-3"]}`))
	}))
	t.Cleanup(server.Close)

	c, err := New(server.URL)
	require.NoError(
		err, "New")

	sender := "alice@example.com"
	dryRun := true
	got, err := c.StageDeletion(context.Background(), &generated.StageDeletionRequestOptions{
		Body: &generated.StageDeletionBody{
			Filter: &generated.StageDeletionFilter{Sender: &sender},
			DryRun: &dryRun,
		},
	})
	require.NoError(
		err, "StageDeletion dry run")

	assert.True(got.DryRun, "dry_run")
	assert.Equal(int64(3), got.MessageCount, "message_count")
	assert.Len(got.SampleGmailIds, 3, "sample ids")
}
