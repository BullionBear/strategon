import type { ArtifactRef } from '$lib/gen/strategyplatform/v1/common_pb';
import { ArtifactType } from '$lib/gen/strategyplatform/v1/common_pb';

export type ArtifactGroup = {
	name: string;
	kind: 'binary' | 'config' | 'other';
	versions: ArtifactRef[]; // newest first; [0] is latest
};

/** Group flat ListArtifacts by name; within each group newest created_at first. */
export function groupArtifacts(artifacts: ArtifactRef[]): ArtifactGroup[] {
	const byName = new Map<string, ArtifactRef[]>();
	for (const a of artifacts) {
		const list = byName.get(a.name) ?? [];
		list.push(a);
		byName.set(a.name, list);
	}
	const groups: ArtifactGroup[] = [];
	for (const [name, versions] of byName) {
		versions.sort((a, b) => createdAtMs(b) - createdAtMs(a));
		groups.push({
			name,
			kind: artifactKind(name),
			versions
		});
	}
	groups.sort((a, b) => a.name.localeCompare(b.name));
	return groups;
}

export function artifactKind(name: string): ArtifactGroup['kind'] {
	if (name.endsWith('-config')) return 'config';
	return 'binary';
}

export function createdAtMs(a: ArtifactRef | undefined): number {
	if (!a?.createdAt) return 0;
	return Number(a.createdAt.seconds) * 1000 + Math.floor(Number(a.createdAt.nanos) / 1e6);
}

/** Newest version for a name, or undefined if none. */
export function latestVersion(artifacts: ArtifactRef[], name: string): string | undefined {
	let best: ArtifactRef | undefined;
	for (const a of artifacts) {
		if (a.name !== name) continue;
		if (!best || createdAtMs(a) > createdAtMs(best)) best = a;
	}
	return best?.version;
}

export function versionsFor(
	artifacts: ArtifactRef[],
	name: string
): { version: string; latest: boolean }[] {
	const list = artifacts
		.filter((a) => a.name === name)
		.sort((a, b) => createdAtMs(b) - createdAtMs(a));
	return list.map((a, i) => ({ version: a.version, latest: i === 0 }));
}

export function truncateDigest(digest: string, head = 12): string {
	if (!digest) return '';
	if (digest.length <= head + 1) return digest;
	const prefix = digest.startsWith('sha256:') ? 'sha256:' : '';
	const rest = digest.slice(prefix.length);
	if (rest.length <= head) return digest;
	return `${prefix}${rest.slice(0, head)}…`;
}

export function relativeTime(a: ArtifactRef | undefined, now = Date.now()): string {
	const ms = createdAtMs(a);
	if (!ms) return '—';
	const sec = Math.max(0, Math.floor((now - ms) / 1000));
	if (sec < 60) return `${sec}s ago`;
	const min = Math.floor(sec / 60);
	if (min < 60) return `${min} min ago`;
	const hr = Math.floor(min / 60);
	if (hr < 48) return `${hr} h ago`;
	const d = Math.floor(hr / 24);
	return `${d} d ago`;
}

export function typeLabel(a: ArtifactRef, kind: ArtifactGroup['kind']): string {
	if (kind === 'config') return 'config';
	switch (a.type) {
		case ArtifactType.OCI_IMAGE:
			return 'oci';
		case ArtifactType.BINARY:
			return 'binary';
		default:
			return 'artifact';
	}
}
