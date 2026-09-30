import { ApiError, csrfToken, streamLines } from "./api";

export type TransferEvent =
  | { t: "tick" }
  | { t: "progress"; rows?: number; bytes?: number; size?: number; object?: string; statements?: number; total?: number; failed?: number }
  | { t: "error"; message: string }
  | {
      t: "done"; ms: number; rows?: number; warnings?: string[];
      file?: { id: string; name: string; size: number; url: string };
      statements?: number; succeeded?: number; failures?: { line: number; message: string; sql: string }[];
    };

export const transferStream = (path: string, body: unknown, signal?: AbortSignal) => streamLines<TransferEvent>(path, body, signal);

export interface Upload {
  id: string;
  name: string;
  size: number;
  format: string;
}

/** Uploads a file to the server's spool, reporting byte progress. */
export function uploadFile(file: File, onProgress: (loaded: number, total: number) => void, signal?: AbortSignal): Promise<Upload> {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("POST", "api/uploads");
    xhr.setRequestHeader("Content-Type", "application/octet-stream");
    xhr.setRequestHeader("X-File-Name", encodeURIComponent(file.name));
    xhr.setRequestHeader("X-CSRF-Token", csrfToken());
    xhr.upload.onprogress = (e) => onProgress(e.loaded, e.lengthComputable ? e.total : file.size);
    xhr.onload = () => {
      let body: any = null;
      try {
        body = JSON.parse(xhr.responseText);
      } catch {
        /* not JSON */
      }
      if (xhr.status >= 200 && xhr.status < 300) resolve(body as Upload);
      else reject(new ApiError(xhr.status, body?.error?.message ?? `Upload failed (${xhr.status})`, body?.error?.code));
    };
    xhr.onerror = () => reject(new ApiError(0, "The upload was interrupted"));
    xhr.onabort = () => reject(new DOMException("Aborted", "AbortError"));
    signal?.addEventListener("abort", () => xhr.abort());
    xhr.send(file);
  });
}

/** Starts a native browser download of a finished spool file. */
export function download(url: string, name: string) {
  const a = document.createElement("a");
  a.href = url;
  a.download = name;
  a.rel = "noopener";
  document.body.appendChild(a);
  a.click();
  a.remove();
}
