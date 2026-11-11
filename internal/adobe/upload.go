package adobe

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"net/http"
	"strings"

	"golang.org/x/image/draw"
)

const uploadImageURL = "https://firefly-3p.ff.adobe.io/v2/storage/image"

func generationSession(a Account) (string, error) {
	id, _, err := TokenIdentity(a.Token)
	if err != nil || id != a.AccountID {
		return "", ErrIdentity
	}
	sherlock, err := Sherlock(a.Cookie)
	if err != nil {
		return "", &Error{Stage: "submit", Code: "session_incomplete"}
	}
	return sherlock, nil
}

// uploadReferences runs only after every input and the account identity have
// passed validation. A failed upload ends the operation before generation.
func (c *Client) uploadReferences(ctx context.Context, a Account, m Model, references []ReferenceImage) ([]referenceBlob, error) {
	blobs := make([]referenceBlob, 0, len(references))
	for index, ref := range references {
		data, _, err := c.request(ctx, "upload", http.MethodPost, uploadImageURL, a, ref.Data, map[string]string{"Content-Type": http.DetectContentType(ref.Data)})
		if err != nil {
			return nil, err
		}
		items, _ := data["images"].([]any)
		if len(items) == 0 {
			return nil, &Error{Stage: "upload", Code: "missing_reference_handle"}
		}
		id := str(object(items[0])["id"])
		if id == "" || len(id) > 2048 || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n\x00") {
			return nil, &Error{Stage: "upload", Code: "invalid_reference_handle"}
		}
		blob := referenceBlob{ID: id, Usage: m.ReferenceUsage}
		if m.ReferenceUsage == "frame" {
			blob.Order = index + 1
		}
		blobs = append(blobs, blob)
	}
	return blobs, nil
}

// The Firefly video composer fits each first/last frame to the requested canvas.
// Center-crop and resize keep that behavior explicit without stretching subjects.
func fitVideoReferences(references []ReferenceImage, width, height int) ([]ReferenceImage, error) {
	out := make([]ReferenceImage, 0, len(references))
	for _, ref := range references {
		source, _, err := image.Decode(bytes.NewReader(ref.Data))
		if err != nil {
			return nil, &Error{Stage: "upload", Code: "invalid_reference_image"}
		}
		bounds := source.Bounds()
		if bounds.Dx() == width && bounds.Dy() == height {
			out = append(out, ref)
			continue
		}
		crop := bounds
		if int64(bounds.Dx())*int64(height) > int64(bounds.Dy())*int64(width) {
			cropWidth := int(int64(bounds.Dy()) * int64(width) / int64(height))
			crop.Min.X += (bounds.Dx() - cropWidth) / 2
			crop.Max.X = crop.Min.X + cropWidth
		} else {
			cropHeight := int(int64(bounds.Dx()) * int64(height) / int64(width))
			crop.Min.Y += (bounds.Dy() - cropHeight) / 2
			crop.Max.Y = crop.Min.Y + cropHeight
		}
		if crop.Dx() < 1 || crop.Dy() < 1 {
			return nil, &Error{Stage: "upload", Code: "invalid_reference_image"}
		}
		target := image.NewNRGBA(image.Rect(0, 0, width, height))
		draw.CatmullRom.Scale(target, target.Bounds(), source, crop, draw.Src, nil)
		var encoded bytes.Buffer
		if png.Encode(&encoded, target) != nil {
			return nil, &Error{Stage: "upload", Code: "reference_conversion_failed"}
		}
		out = append(out, ReferenceImage{Data: encoded.Bytes(), MIME: "image/png"})
	}
	return out, nil
}
