package teams

import (
	"encoding/json"
	"testing"

	"github.com/lao/botbooter/internal/asserts"
	"github.com/lao/botbooter/internal/core"
)

func TestAttachments(t *testing.T) {
	body := `{"type":"message","attachments":[{"contentType":"image/png","contentUrl":"https://x/i.png","name":"i.png"}]}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal activity")
	// Attachments reads the parse-time attachments carried on the Message, so go
	// through toMessage rather than hand-building a Message with only Raw.
	m := toMessage(&act, json.RawMessage(body))
	atts, err := (&adapter{}).Attachments(m)
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.True(t, atts[0].IsImage, "image attachment flagged")
	asserts.Equal(t, atts[0].URL, "https://x/i.png", "attachment URL")
}

func TestAttachments_FileDownloadInfo(t *testing.T) {
	// An uploaded PNG: contentUrl is a SharePoint page, content.downloadUrl is the
	// directly fetchable link, and the image flag must come from content.fileType
	// (the wrapper contentType is the generic file-download-info type).
	body := `{"type":"message","attachments":[{` +
		`"contentType":"application/vnd.microsoft.teams.file.download.info",` +
		`"contentUrl":"https://contoso.sharepoint.com/personal/x/pic.png",` +
		`"name":"pic.png",` +
		`"content":{"downloadUrl":"https://download.example/pic.png?tempauth=abc","fileType":"PNG"}}]}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal activity")
	atts, err := (&adapter{}).Attachments(toMessage(&act, json.RawMessage(body)))
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.Equal(t, atts[0].URL, "https://download.example/pic.png?tempauth=abc", "downloadUrl used, not SharePoint contentUrl")
	asserts.True(t, atts[0].IsImage, "image flagged from content.fileType")
}

func TestAttachments_FileDownloadInfoMissingContent(t *testing.T) {
	// Teams sometimes omits the content object on channel uploads; fall back to the
	// contentUrl rather than erroring or emitting an empty URL.
	body := `{"type":"message","attachments":[{` +
		`"contentType":"application/vnd.microsoft.teams.file.download.info",` +
		`"contentUrl":"https://contoso.sharepoint.com/personal/x/report.pdf","name":"report.pdf"}]}`
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(body), &act), "unmarshal activity")
	atts, err := (&adapter{}).Attachments(toMessage(&act, json.RawMessage(body)))
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 1, "one attachment")
	asserts.Equal(t, atts[0].URL, "https://contoso.sharepoint.com/personal/x/report.pdf", "falls back to contentUrl when content absent")
	asserts.False(t, atts[0].IsImage, "non-image upload not flagged")
}

func TestAttachments_NonTeams(t *testing.T) {
	atts, err := (&adapter{}).Attachments(&core.Message{Raw: "not-a-teams-message"})
	asserts.NoError(t, err, "non-Teams message")
	asserts.True(t, atts == nil, "nil attachments for non-Teams message")
}

func TestAttachments_None(t *testing.T) {
	var act inboundActivity
	asserts.NoError(t, json.Unmarshal([]byte(`{"type":"message"}`), &act), "unmarshal activity")
	m := toMessage(&act, json.RawMessage(`{"type":"message"}`))
	atts, err := (&adapter{}).Attachments(m)
	asserts.NoError(t, err, "Attachments")
	asserts.Equal(t, len(atts), 0, "no attachments")
}
