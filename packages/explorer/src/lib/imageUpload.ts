// RFC BO image-upload helpers for the explorer authoring UI.

// IMAGE_MEDIA_TYPES is the accepted set — mirrors the backend whitelist
// (validImageMediaTypes / ValidImageMediaType in document.go). SVG is excluded
// (script-in-SVG XSS surface on the serving endpoint).
export const IMAGE_MEDIA_TYPES = ["image/png", "image/jpeg", "image/gif", "image/webp"];

// isSupportedImage reports whether a File/Blob's type is an accepted image.
export function isSupportedImage(type: string): boolean {
  return IMAGE_MEDIA_TYPES.includes(type);
}

// readImageAsBase64 reads a File/Blob into standard base64 (NO data: prefix — the
// set_asset op wants the raw base64, matching the backend's decode). Rejects an
// unsupported media type up front so the server never sees it.
export function readImageAsBase64(file: File | Blob): Promise<{ mediaType: string; data: string }> {
  return new Promise((resolve, reject) => {
    if (!isSupportedImage(file.type)) {
      reject(new Error(`unsupported image type "${file.type || "unknown"}" (png, jpeg, gif, webp only)`));
      return;
    }
    const reader = new FileReader();
    reader.onerror = () => reject(reader.error ?? new Error("failed to read file"));
    reader.onload = () => {
      // FileReader.readAsDataURL yields "data:<type>;base64,<payload>"; strip the
      // prefix to hand set_asset the raw base64.
      const result = String(reader.result ?? "");
      const comma = result.indexOf(",");
      if (comma < 0) {
        reject(new Error("could not decode image data"));
        return;
      }
      resolve({ mediaType: file.type, data: result.slice(comma + 1) });
    };
    reader.readAsDataURL(file);
  });
}

// imageFileFromPaste extracts the first supported image from a paste event's
// clipboard items, or null if none (so a normal text paste is ignored).
export function imageFileFromPaste(items: DataTransferItemList | null | undefined): File | null {
  if (!items) return null;
  for (let i = 0; i < items.length; i++) {
    const it = items[i];
    if (it.kind === "file" && isSupportedImage(it.type)) {
      const f = it.getAsFile();
      if (f) return f;
    }
  }
  return null;
}
