package filehandler

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/jhillyerd/enmime"
	models "github.com/microsoftgraph/msgraph-sdk-go/models"
)

// ExportToRecipient is an exported version of toRecipient for testing.
func ExportToRecipient(mRecipient models.Recipientable) Recipient {
	return toRecipient(mRecipient)
}

// ExportExtractFilesFromEnvelope is an exported version of extractFilesFromEnvelope for testing.
func (fh *FileHandler) ExportExtractFilesFromEnvelope(root *os.Root, env *enmime.Envelope, parentSequence int) []AttachmentMetadata {
	return fh.extractFilesFromEnvelope(root, env, parentSequence)
}

// ExportGetMutex is an exported version of getMutex for testing.
func (fh *FileHandler) ExportGetMutex(filePath string) *sync.Mutex {
	return fh.getMutex(filePath)
}

// ExportCopyWithContext is an exported version of copyWithContext for testing.
func ExportCopyWithContext(ctx context.Context, dst io.Writer, src io.Reader) error {
	return copyWithContext(ctx, dst, src)
}
