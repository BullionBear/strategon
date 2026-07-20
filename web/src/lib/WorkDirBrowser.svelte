<script lang="ts">
	import { onMount } from 'svelte';
	import type { DirEntry } from '$lib/gen/strategyplatform/v1/agent_service_pb';
	import { formatBytes, formatClock } from '$lib/fleet';
	import {
		MIN_FILE_BROWSE_AGENT_VERSION,
		browseDir,
		downloadFiles,
		joinPath,
		pathCrumbs
	} from '$lib/workdir';

	interface Props {
		machineId: string;
		strategy: string;
		reachable: boolean;
		agentVersion: number;
	}

	let { machineId, strategy, reachable, agentVersion }: Props = $props();

	let path = $state('.');
	let entries = $state<DirEntry[]>([]);
	let selected = $state<Set<string>>(new Set());
	let loading = $state(false);
	let downloading = $state(false);
	let error = $state('');
	let progressBytes = $state(0);
	let progressName = $state('');

	const supported = $derived(agentVersion >= MIN_FILE_BROWSE_AGENT_VERSION);
	const crumbs = $derived(pathCrumbs(path));
	const allSelected = $derived(
		entries.length > 0 && entries.every((e) => selected.has(joinPath(path, e.name)))
	);

	let loadAc: AbortController | null = null;
	let dlAc: AbortController | null = null;

	async function load(nextPath: string) {
		if (!reachable || !supported) return;
		loadAc?.abort();
		loadAc = new AbortController();
		loading = true;
		error = '';
		selected = new Set();
		try {
			const res = await browseDir(machineId, strategy, nextPath, loadAc.signal);
			path = res.path || nextPath || '.';
			entries = [...res.entries].sort((a, b) => {
				if (a.isDir !== b.isDir) return a.isDir ? -1 : 1;
				return a.name.localeCompare(b.name);
			});
		} catch (e) {
			if ((e as Error)?.name === 'AbortError') return;
			error = e instanceof Error ? e.message : String(e);
			entries = [];
		} finally {
			loading = false;
		}
	}

	function toggle(name: string) {
		const key = joinPath(path, name);
		const next = new Set(selected);
		if (next.has(key)) next.delete(key);
		else next.add(key);
		selected = next;
	}

	function toggleAll() {
		if (allSelected) {
			selected = new Set();
			return;
		}
		selected = new Set(entries.map((e) => joinPath(path, e.name)));
	}

	async function openDir(name: string) {
		await load(joinPath(path, name));
	}

	async function goCrumb(p: string) {
		await load(p);
	}

	async function doDownload() {
		if (selected.size === 0 || downloading) return;
		dlAc?.abort();
		dlAc = new AbortController();
		downloading = true;
		error = '';
		progressBytes = 0;
		progressName = '';
		try {
			await downloadFiles(machineId, strategy, [...selected], {
				signal: dlAc.signal,
				onProgress: (p) => {
					progressBytes = p.bytes;
					progressName = p.filename;
				}
			});
		} catch (e) {
			if ((e as Error)?.name === 'AbortError') return;
			error = e instanceof Error ? e.message : String(e);
		} finally {
			downloading = false;
		}
	}

	function entryType(e: DirEntry): string {
		// Symlinks are never navigable (agent does not follow them for browse).
		if (e.isSymlink) return 'symlink';
		if (e.isDir) return 'dir';
		return 'file';
	}

	function entryLabel(e: DirEntry): string {
		if (e.isSymlink) return `${e.name} →`;
		if (e.isDir) return `${e.name}/`;
		return e.name;
	}

	onMount(() => {
		return () => {
			loadAc?.abort();
			dlAc?.abort();
		};
	});

	$effect(() => {
		const _m = machineId;
		const _s = strategy;
		const _r = reachable;
		const _ok = supported;
		if (_r && _ok && _m && _s) {
			load('.');
		}
	});
</script>

<div class="workdir panel">
	<div class="toolbar">
		<div>
			<h2>WorkDir files</h2>
			<p class="muted tiny">Browse and download files under this strategy's working directory</p>
		</div>
		<div class="actions">
			<button
				class="btn"
				disabled={!reachable || !supported || selected.size === 0 || downloading}
				onclick={doDownload}
			>
				{downloading ? 'Downloading…' : `Download${selected.size ? ` (${selected.size})` : ''}`}
			</button>
		</div>
	</div>

	{#if !reachable}
		<p class="pill bad">Machine unreachable — connect the agent to browse files.</p>
	{:else if !supported}
		<p class="pill lag">
			Agent version {agentVersion} does not support file browse (need ≥ {MIN_FILE_BROWSE_AGENT_VERSION}).
		</p>
	{:else}
		<nav class="crumbs" aria-label="Path">
			{#each crumbs as c, i}
				{#if i > 0}<span class="sep">/</span>{/if}
				<button type="button" class="crumb" class:current={i === crumbs.length - 1} onclick={() => goCrumb(c.path)}>
					{c.label}
				</button>
			{/each}
			{#if loading}
				<span class="muted tiny" style="margin-left:0.75rem">loading…</span>
			{/if}
		</nav>

		{#if downloading && progressName}
			<p class="muted tiny mono">
				{progressName} · {formatBytes(BigInt(progressBytes))}
			</p>
		{/if}

		{#if error}
			<p class="pill bad">{error}</p>
		{/if}

		<div class="table-wrap">
			<table class="fleet-table">
				<thead>
					<tr>
						<th class="check">
							<input
								type="checkbox"
								checked={allSelected}
								disabled={entries.length === 0}
								onchange={toggleAll}
								aria-label="Select all"
							/>
						</th>
						<th>Name</th>
						<th>Size</th>
						<th>Modified</th>
						<th>Type</th>
					</tr>
				</thead>
				<tbody>
					{#each entries as e}
						{@const key = joinPath(path, e.name)}
						<tr>
							<td class="check">
								<input
									type="checkbox"
									checked={selected.has(key)}
									onchange={() => toggle(e.name)}
									aria-label="Select {e.name}"
								/>
							</td>
							<td class="mono name">
								{#if e.isDir && !e.isSymlink}
									<button type="button" class="linkish" onclick={() => openDir(e.name)}
										>{entryLabel(e)}</button
									>
								{:else}
									<span class:symlink={e.isSymlink} title={e.isSymlink ? 'Symlink (not followed)' : undefined}
										>{entryLabel(e)}</span
									>
								{/if}
							</td>
							<td class="mono muted">{e.isDir || e.isSymlink ? '—' : formatBytes(e.size)}</td>
							<td class="mono muted tiny">{e.modTime ? formatClock(e.modTime) : '—'}</td>
							<td class="muted tiny">{entryType(e)}</td>
						</tr>
					{:else}
						<tr>
							<td colspan="5" class="muted">Empty directory</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</div>

<style>
	.workdir {
		margin-top: 1.5rem;
	}
	.toolbar {
		display: flex;
		justify-content: space-between;
		align-items: flex-start;
		gap: 1rem;
		flex-wrap: wrap;
	}
	.toolbar h2 {
		margin: 0;
		font-size: 1.15rem;
	}
	.toolbar .tiny {
		margin: 0.2rem 0 0;
	}
	.crumbs {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.15rem;
		margin: 0.85rem 0 0.65rem;
		font-family: var(--mono);
		font-size: 0.85em;
	}
	.crumb {
		background: none;
		border: none;
		padding: 0;
		color: var(--accent-ink);
		cursor: pointer;
		font: inherit;
	}
	.crumb:hover {
		text-decoration: underline;
	}
	.crumb.current {
		color: var(--ink);
		cursor: default;
		font-weight: 600;
	}
	.crumb.current:hover {
		text-decoration: none;
	}
	.sep {
		color: var(--ink-muted);
		margin: 0 0.1rem;
	}
	.table-wrap {
		overflow-x: auto;
	}
	.check {
		width: 2rem;
	}
	.name {
		word-break: break-all;
	}
	.linkish {
		background: none;
		border: none;
		padding: 0;
		color: var(--accent-ink);
		cursor: pointer;
		font: inherit;
		text-align: left;
	}
	.linkish:hover {
		text-decoration: underline;
	}
	.symlink {
		color: var(--ink-muted);
		font-style: italic;
	}
	.pill {
		display: inline-block;
		margin-top: 0.75rem;
	}
</style>
