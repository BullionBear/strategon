import { client } from '$lib/api';
import type { DirEntry } from '$lib/gen/strategyplatform/v1/agent_service_pb';
import { TransferKind } from '$lib/gen/strategyplatform/v1/agent_service_pb';

export const MIN_FILE_BROWSE_AGENT_VERSION = 2;

export type DownloadProgress = {
	bytes: number;
	filename: string;
	transferKind: TransferKind;
};

/** Join a WorkDir-relative path. */
export function joinPath(base: string, name: string): string {
	if (!base || base === '.' || base === '/') return name;
	return `${base.replace(/\/+$/, '')}/${name}`;
}

/** Parent of a WorkDir-relative path; '.' for root. */
export function parentPath(p: string): string {
	if (!p || p === '.' || p === '/') return '.';
	const trimmed = p.replace(/\/+$/, '');
	const i = trimmed.lastIndexOf('/');
	if (i <= 0) return '.';
	return trimmed.slice(0, i);
}

/** Breadcrumb segments for a relative path. */
export function pathCrumbs(p: string): { label: string; path: string }[] {
	const crumbs: { label: string; path: string }[] = [{ label: 'WorkDir', path: '.' }];
	if (!p || p === '.' || p === '/') return crumbs;
	const parts = p.replace(/^\/+|\/+$/g, '').split('/').filter(Boolean);
	let cur = '';
	for (const part of parts) {
		cur = cur ? `${cur}/${part}` : part;
		crumbs.push({ label: part, path: cur });
	}
	return crumbs;
}

export async function browseDir(
	machineId: string,
	strategy: string,
	path: string,
	signal?: AbortSignal
): Promise<{ entries: DirEntry[]; path: string }> {
	const res = await client.browseDir({ machineId, strategy, path }, { signal });
	return { entries: res.entries, path: res.path || path || '.' };
}

/**
 * Stream DownloadFiles into a Blob and trigger a browser download.
 * Reports progress via onProgress; cancels when signal aborts.
 */
export async function downloadFiles(
	machineId: string,
	strategy: string,
	paths: string[],
	opts?: {
		signal?: AbortSignal;
		onProgress?: (p: DownloadProgress) => void;
	}
): Promise<{ filename: string; bytes: number; transferKind: TransferKind }> {
	const chunks: Uint8Array[] = [];
	let filename = 'download';
	let transferKind = TransferKind.UNSPECIFIED;
	let bytes = 0;

	const stream = client.downloadFiles({ machineId, strategy, paths }, { signal: opts?.signal });
	for await (const chunk of stream) {
		if (chunk.filename) filename = chunk.filename;
		if (chunk.transferKind !== TransferKind.UNSPECIFIED) {
			transferKind = chunk.transferKind;
		}
		if (chunk.data?.length) {
			chunks.push(chunk.data);
			bytes += chunk.data.length;
			opts?.onProgress?.({ bytes, filename, transferKind });
		}
		if (chunk.eof) break;
	}

	const blob = new Blob(chunks as BlobPart[]);
	triggerSave(blob, filename);
	return { filename, bytes, transferKind };
}

function triggerSave(blob: Blob, filename: string) {
	const url = URL.createObjectURL(blob);
	const a = document.createElement('a');
	a.href = url;
	a.download = filename;
	a.rel = 'noopener';
	document.body.appendChild(a);
	a.click();
	a.remove();
	URL.revokeObjectURL(url);
}
