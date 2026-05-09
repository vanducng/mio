# Attachment-downloader IAM setup

One-time runbook to create the GCS service account and Workload Identity
binding for the `mio-attachment-downloader` sidecar.

## Prerequisites

- `gcloud` authenticated with `roles/iam.serviceAccountAdmin` on project
  `dp-prod-7e26`.
- `roles/storage.admin` on bucket `gs://ab-spectrum-backups-prod` (for the
  prefix-scoped binding step).
- GKE cluster with Workload Identity enabled; namespace `mio` exists.

## Steps

### 1. Create the GSA

```bash
gcloud iam service-accounts create mio-attachments \
  --display-name="mio attachment downloader" \
  --project=dp-prod-7e26
```

### 2. Grant `roles/storage.objectAdmin` scoped to the prefix

The IAM Condition restricts the binding to objects under
`mio/attachments/`, so the sidecar can never touch other prefixes
(e.g. existing `cnpg/` CNPG backups).

```bash
gcloud storage buckets add-iam-policy-binding gs://ab-spectrum-backups-prod \
  --member=serviceAccount:mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/storage.objectAdmin \
  --condition='expression=resource.name.startsWith("projects/_/buckets/ab-spectrum-backups-prod/objects/mio/attachments/"),title=mio_attachments_prefix_only,description=Restrict to the mio attachments prefix'
```

### 3. Grant `roles/iam.serviceAccountTokenCreator` on self (V4 signed-URL signing)

GCS V4 signed URLs require the runtime identity to be able to sign blobs
on behalf of itself.

```bash
gcloud iam service-accounts add-iam-policy-binding \
  mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/iam.serviceAccountTokenCreator \
  --member=serviceAccount:mio-attachments@dp-prod-7e26.iam.gserviceaccount.com
```

### 4. Workload Identity binding KSA → GSA

```bash
gcloud iam service-accounts add-iam-policy-binding \
  mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="serviceAccount:dp-prod-7e26.svc.id.goog[mio/mio-attachment-downloader]"
```

### 5. Verify

After the chart is deployed:

```bash
kubectl -n mio get sa mio-attachment-downloader -o yaml | grep iam.gke.io
# expected: iam.gke.io/gcp-service-account: mio-attachments@dp-prod-7e26.iam.gserviceaccount.com
```

## Rollback

```bash
gcloud storage buckets remove-iam-policy-binding gs://ab-spectrum-backups-prod \
  --member=serviceAccount:mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --role=roles/storage.objectAdmin

gcloud iam service-accounts delete \
  mio-attachments@dp-prod-7e26.iam.gserviceaccount.com \
  --project=dp-prod-7e26
```
