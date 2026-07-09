// Attachment mapping: Bot Framework attachments to botbooter's core.Attachment.

package teams

import (
	"strings"

	"github.com/lao/botbooter/internal/core"
)

// Attachments returns the files attached to m, mapped from the Activity's
// attachments (decoded once at parse time). The error return is kept for the
// core.Adapter contract but is always nil.
func (a *adapter) Attachments(m *core.Message) ([]core.Attachment, error) {
	tm, ok := RawMessage(m)
	if !ok || tm == nil {
		return nil, nil
	}
	if len(tm.attachments) == 0 {
		return []core.Attachment{}, nil
	}
	out := make([]core.Attachment, 0, len(tm.attachments))
	for _, at := range tm.attachments {
		out = append(out, toAttachment(at))
	}
	return out, nil
}

// toAttachment maps one Bot Framework attachment to the platform-agnostic form. A
// Teams uploaded file (fileDownloadInfoType) uses content.downloadUrl and takes its
// image flag from content.fileType; any other attachment uses contentUrl and its
// image/* contentType directly.
func toAttachment(at activityAttachment) core.Attachment {
	if at.ContentType == fileDownloadInfoType {
		// Fall back to the top-level contentUrl (a SharePoint page) when the content
		// object is omitted, a known Teams channel-upload quirk.
		url := at.Content.DownloadURL
		if url == "" {
			url = at.ContentURL
		}
		return core.Attachment{
			IsImage: imageFileExts[strings.ToLower(at.Content.FileType)],
			URL:     url,
		}
	}
	return core.Attachment{
		IsImage: strings.HasPrefix(at.ContentType, "image/"),
		URL:     at.ContentURL,
	}
}

// fileDownloadInfoType is the contentType Teams uses for an uploaded file, whose
// content.downloadUrl is the directly fetchable link.
const fileDownloadInfoType = "application/vnd.microsoft.teams.file.download.info"

// imageFileExts are the uploaded-file extensions classified as images.
var imageFileExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true, "webp": true, "bmp": true,
}

type activityAttachment struct {
	ContentType string `json:"contentType"`
	ContentURL  string `json:"contentUrl"`
	// Content is the FileDownloadInfo object on uploaded-file attachments; empty otherwise.
	Content attachmentContent `json:"content"`
}

// attachmentContent is the content object of a FileDownloadInfo attachment:
// DownloadURL is the directly fetchable link, FileType the extension.
type attachmentContent struct {
	DownloadURL string `json:"downloadUrl"`
	FileType    string `json:"fileType"`
}
