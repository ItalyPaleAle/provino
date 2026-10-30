# Storage

provino keeps two stores: one for the **originals** it receives, and one for the **variants** it generates along with each image's manifest. They're separate because they usually differ in visibility and lifetime: the variants are what browsers download, often straight from the store or through a CDN, while the originals are typically private.

```yaml
storage:
  originals: s3://<access-key>:<secret-key>@s3.us-east-1.amazonaws.com/originals
  variants: s3://<access-key>:<secret-key>@s3.us-east-1.amazonaws.com/assets?prefix=images
```

| Backend | Connection string |
| --- | --- |
| Local file system | `/path/to/folder`, `fs:/path/to/folder`, or `local:/path/to/folder`, optionally followed by `?fileMode=<mode>&dirMode=<mode>` |
| S3, or an S3-compatible store | `s3://<access-key>:<secret-key>@s3.<region>.amazonaws.com/<bucket>?prefix=<key-prefix>&sessionToken=<token>` |
| Vercel Blob, with a read-write token | `vercelblob:?tokenEnv=<environment-variable>&access=<private\|public>&prefix=<key-prefix>` |
| Vercel Blob, with OIDC | `vercelblob:?auth=oidc&storeIdEnv=<environment-variable>&access=<private\|public>&prefix=<key-prefix>` |

A `prefix` is applied by the backend to every key, so the keys below, and the keys in a manifest, are always relative to it.

On the local file system, files are written as `0600` and folders as `0750` by default. To serve the variants straight from the folder with a web server that runs as another user, give them wider permissions in the DSN, for example `fs:/var/lib/provino/data/variants?fileMode=0644&dirMode=0755`.

provino doesn't serve images. Serve the variants from the store, using the keys in the manifest.

## Object layout

Every object of an image is stored under its namespace and two folder levels taken from the first four characters of its ID, so no folder holds every image of a namespace:

| Object | Store | Key |
| --- | --- | --- |
| Original | originals | `<namespace>/<id[0:2]>/<id[2:4]>/<imageId>_orig.<ext>` |
| Variant | variants | `<namespace>/<id[0:2]>/<id[2:4]>/<imageId>_<width>x<height>.<ext>` |
| Manifest | variants | `<namespace>/<id[0:2]>/<id[2:4]>/<imageId>_manifest.json` |

For example, `default/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_600x400.webp`.

- A variant's key is its actual size and format. Two variants of the same size and format are the same object, which is why a request can't ask for both with different encoder settings.
- The extension of an original is its detected format: `jpg`, `png`, `webp`, `avif`, `heic` or `heif`.
- New image IDs are UUIDv4s, so their leading characters spread evenly across the ~65,000 folders.

The layout isn't configurable. Applications that compute keys in Go can use the [`imagekeys`](../imagekeys) package, but most commonly you will just load the keys from the manifest.

> While the manifest doesn't contain secrets, if you're starting a file server / CDN directly from the variants storage whenever possible you may want to block public access to `.json` files to prevent serving the manifest.

## The manifest

Each image has a manifest next to its variants, which is rewritten by every job that completes with variants (`completed`). A job that fails puts back the manifest it found.

```json
{
  "version": "provino/v1",
  "namespace": "default",
  "imageId": "01a061a9-7384-4e97-aadf-232d1e006b83",
  "status": "completed",
  "jobId": "019a7c3e-5b2d-7f41-9c8a-2d4e6f8a0b1c",
  "sourceKey": "default/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_orig.jpg",
  "sourceFormat": "jpg",
  "sourceSize": 482113,
  "sourceWidth": 2400,
  "sourceHeight": 1600,
  "storedAt": "2026-10-11T10:11:10Z",
  "requestedAt": "2026-10-11T10:11:12Z",
  "startedAt": "2026-10-11T10:11:12Z",
  "completedAt": "2026-10-11T10:11:14Z",
  "variants": [
    {
      "key": "default/01/a0/01a061a9-7384-4e97-aadf-232d1e006b83_600x400.webp",
      "name": "1200x400.webp",
      "format": "webp",
      "quality": 80,
      "effort": 5,
      "requestedResolution": { "width": 1200, "height": 400 },
      "fit": "contain",
      "width": 600,
      "height": 400,
      "size": 38122
    }
  ]
}
```

| Field | Meaning |
| --- | --- |
| `version` | Always `provino/v1`. Check it before trusting the rest; Go applications can use the [`manifest`](../manifest) package's `Parse` |
| `status` | `stored` before any job generated variants, `completed` after |
| `jobId` | The job that wrote the manifest |
| `sourceKey` | Key of the original, in the originals store |
| `sourceWidth`, `sourceHeight` | The original's size after its EXIF orientation |
| `variants` | Every variant the last job asked for, in the order it asked for them. Every file listed is in the store |
| `variants[].key` | Key of the file, in the variants store |
| `variants[].requestedResolution` | The box that was asked for. `width` and `height` are what the file came out at |

### Variants are never enlarged

No variant is larger than its original. A box the original can't fill gets the original's own pixels:

- `contain`: the whole original
- `cover`: the largest centered crop of the box's shape

Every such box of one fit and format produces the same file, so only the tightest one stores it. The others are listed with `"skipped": true` and `skippedFor`, the box that holds the file, and they repeat that file's key, width, height and size. A reader that doesn't know about skipped entries sees a duplicate and drops it.

| Original | Boxes (`contain`) | Files |
| --- | --- | --- |
| 800x800 | 1000x1000, 2000x2000 | 800x800 for 1000x1000; 2000x2000 is skipped |
| 1200x800 | 500x500, 1400x1400, 2048x2048 | 500x333, and 1200x800 for 1400x1400; 2048x2048 is skipped |

### Changing an image's variants

`POST /v1/client/optimize` takes every variant an image should have, including the ones it already has. Stored files that still match their box, fit and format are reused rather than encoded again, whatever encoder settings produced them. Re-sending the whole set is a no-op for the variants that are already there, and the manifest always lists the complete set.

Variants that drop out of the set stay in the store, because an application may still reference them. Removing objects is left to the operator.

## Originals and IDs

An image ID is spent by its first upload: an upload whose ID is already stored, in any format, is refused with `409 image_exists`. So is an upload of an ID that another upload is storing at the same time, or that a job is working on.

An optimize request for an ID that an upload is still storing is refused with `409 upload_in_progress`, and can be sent again once the upload has returned. Originals are kept after their jobs complete, because every future optimize request reads them again.
