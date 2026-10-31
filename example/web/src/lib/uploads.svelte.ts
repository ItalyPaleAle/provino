// Uploads to provino's public ingest endpoint, and the jobs they start
import { auth } from "./session.svelte";
import type { ApiError, Config, JobAccepted, JobStatus } from "./types";

export interface Upload {
  id: number;
  file: string;
  profile: string;
  progress: number;
  state: "uploading" | "running" | "done" | "failed";
  message: string;
  accepted?: JobAccepted;
}

// The uploads of this tab, newest first
export const uploads = $state<Upload[]>([]);

let nextID = 1;

// Upload one file, and follow its job until it finishes, calling settled then
// The metadata part comes first, and its context is what provino forwards to the app's authorizer
export function startUpload(config: Config, file: File, profile: string, settled: () => void): void {
  uploads.unshift({ id: nextID++, file: file.name, profile, progress: 0, state: "uploading", message: "Uploading" });
  // The array's items are reactive proxies, so the upload is updated through the one in the array
  const upload = uploads[0];

  const form = new FormData();
  form.append("metadata", JSON.stringify({ context: { profile } }));
  form.append("image", file, file.name);

  // XMLHttpRequest rather than fetch, because it reports the upload's progress
  const xhr = new XMLHttpRequest();
  xhr.open("POST", `${config.provinoUrl}/v1/public/ingest`);
  xhr.setRequestHeader("Authorization", `Bearer ${auth.session?.token}`);
  xhr.upload.onprogress = (e) => {
    if (e.lengthComputable) {
      upload.progress = (e.loaded / e.total) * 100;
      upload.message = `Uploading, ${Math.round(upload.progress)}%`;
    }
  };
  xhr.onload = () => {
    upload.progress = 100;
    let body: JobAccepted & ApiError = {} as JobAccepted & ApiError;
    try {
      body = JSON.parse(xhr.responseText);
    } catch {
      // An answer that isn't JSON is reported by its status alone
    }
    if (xhr.status !== 202) {
      upload.state = "failed";
      upload.message = `${xhr.status} ${body.code ?? ""}: ${body.message ?? xhr.statusText}${body.innerError ? ` (${body.innerError})` : ""}`;
      settled();
      return;
    }
    upload.accepted = body;
    upload.state = "running";
    upload.message = "Stored, and the job is running";
    void follow(config, upload, settled);
  };
  xhr.onerror = () => {
    upload.state = "failed";
    upload.message = `provino couldn't be reached at ${config.provinoUrl}. Is it running, with this page's origin in server.corsAllowedOrigins?`;
    settled();
  };
  xhr.send(form);
}

// Poll the job's status on provino until it finishes
// By then provino has delivered the job's webhook, so the app's images show the outcome
async function follow(config: Config, upload: Upload, settled: () => void): Promise<void> {
  const { namespace, imageId, jobId } = upload.accepted!;
  const url = `${config.provinoUrl}/v1/public/images/${namespace}/${imageId}/jobs/${jobId}`;
  for (;;) {
    await new Promise((resolve) => setTimeout(resolve, 1000));
    const res = await fetch(url, { headers: { Authorization: `Bearer ${auth.session?.token}` } });
    if (res.status === 429) {
      continue;
    }
    const status = (await res.json()) as JobStatus & ApiError;
    if (!res.ok) {
      upload.state = "failed";
      upload.message = `Couldn't read the job's status: ${res.status} ${status.code ?? ""}`;
      break;
    }

    if (status.status === "running") {
      upload.message = `Job running, ${status.completed} of ${status.total} variants done`;
      continue;
    }
    upload.state = status.status === "completed" ? "done" : "failed";
    upload.message = status.status === "completed" ? `Completed: ${status.total} variants` : `Failed: ${status.error}`;
    break;
  }
  settled();
}
