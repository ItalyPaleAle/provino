// The shapes the example app's API and provino answer with

export interface Profile {
  id: string;
  label: string;
  description: string;
}

export interface Config {
  provinoUrl: string;
  clientId: string;
  profiles: Profile[];
}

export interface Resolution {
  width: number;
  height: number;
}

// One variant in a provino/v1 manifest
export interface Variant {
  key: string;
  format: string;
  quality?: number;
  effort?: number;
  requestedResolution: Resolution;
  fit: string;
  width: number;
  height: number;
  size: number;
  skipped?: boolean;
  skippedFor?: Resolution;
}

export interface Manifest {
  version: string;
  namespace: string;
  imageId: string;
  status: string;
  jobId?: string;
  sourceKey: string;
  sourceFormat: string;
  sourceSize?: number;
  sourceWidth?: number;
  sourceHeight?: number;
  variants: Variant[];
}

export type EventType = "image.stored" | "image.optimized" | "image.failed";

export interface ImageRecord {
  namespace: string;
  imageId: string;
  owner: string;
  profile?: string;
  status: EventType;
  jobId: string;
  error?: string;
  manifest?: Manifest;
  updatedAt: string;
}

// A request provino made to the app's authorizer, and the app's answer
export interface AuthorizationRecord {
  id: number;
  receivedAt: string;
  subject: string;
  requestHeaders: Record<string, string>;
  request: unknown;
  status: number;
  response: unknown;
}

// A webhook delivery provino made to the app, and what the app did with it
export interface DeliveryRecord {
  id: number;
  receivedAt: string;
  webhookId: string;
  type: EventType;
  namespace: string;
  imageId: string;
  jobId: string;
  timestamp: string;
  requestHeaders: Record<string, string>;
  body: unknown;
  status: number;
  outcome: string;
}

// What provino answers an upload with
export interface JobAccepted {
  namespace: string;
  imageId: string;
  jobId: string;
  sourceKey?: string;
  manifestKey: string;
  status: string;
}

// The status of a job on provino
export interface JobStatus {
  jobId: string;
  status: "running" | "completed" | "failed";
  total: number;
  completed: number;
  error?: string;
}

// An error from provino or the app
export interface ApiError {
  code?: string;
  message?: string;
  innerError?: string;
}
