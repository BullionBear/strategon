<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { SharedFileView } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import type { ArtifactRef } from '$lib/gen/strategyplatform/v1/common_pb';
	import { truncateDigest } from '$lib/artifacts';

	interface Props {
		machineId: string;
	}
	let { machineId }: Props = $props();

	let files = $state<SharedFileView[]>([]);
	let artifacts = $state<ArtifactRef[]>([]);
	let error = $state('');
	let busy = $state(false);
	let editing = $state(false);
	let draftName = $state('');
	let draftVersion = $state('');
	let draftRows = $state<{ name: string; version: string }[]>([]);

	const versionsForName = $derived(
		artifacts
			.filter((a) => a.name === draftName)
			.map((a) => a.version)
			.filter((v, i, arr) => arr.indexOf(v) === i)
	);

	const artifactNames = $derived(
		[...new Set(artifacts.map((a) => a.name))].sort((a, b) => a.localeCompare(b))
	);

	async function load() {
		if (!machineId) return;
		try {
			const [sf, arts] = await Promise.all([
				client.listSharedFiles({ machineId }),
				client.listArtifacts({})
			]);
			files = sf.files;
			artifacts = arts.artifacts;
			error = '';
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	onMount(() => {
		load();
		const t = setInterval(load, 10000);
		return () => clearInterval(t);
	});

	$effect(() => {
		if (machineId) load();
	});

	function beginEdit() {
		draftRows = files.map((f) => ({ name: f.name, version: f.desiredVersion }));
		draftName = '';
		draftVersion = '';
		editing = true;
		error = '';
	}

	function addRow() {
		if (!draftName || !draftVersion) {
			error = 'Name and version are required';
			return;
		}
		if (draftRows.some((r) => r.name === draftName)) {
			error = `Duplicate name ${draftName}`;
			return;
		}
		draftRows = [...draftRows, { name: draftName, version: draftVersion }];
		draftName = '';
		draftVersion = '';
		error = '';
	}

	function removeRow(name: string) {
		draftRows = draftRows.filter((r) => r.name !== name);
	}

	async function save() {
		busy = true;
		error = '';
		try {
			await client.setSharedFiles({
				machineId,
				files: draftRows.map((r) => ({ name: r.name, artifactVersion: r.version }))
			});
			editing = false;
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="shared">
	<div class="head">
		<h2>Shared files</h2>
		{#if !editing}
			<button class="btn secondary" onclick={beginEdit}>Edit</button>
		{/if}
	</div>
	<p class="muted">
		Machine-level reference data under <span class="mono">./shared/</span>. Updates take effect
		on next process start.
	</p>

	{#if error}
		<p class="pill bad">{error}</p>
	{/if}

	{#if editing}
		<div class="panel edit" style="margin-top:0.75rem">
			{#if draftRows.length === 0}
				<p class="muted">No files. Add one below, or save empty to clear.</p>
			{:else}
				<ul class="list">
					{#each draftRows as row (row.name)}
						<li>
							<span class="mono">{row.name}</span>
							<span class="muted mono">@ {row.version}</span>
							<button class="btn danger" type="button" onclick={() => removeRow(row.name)}>
								Remove
							</button>
						</li>
					{/each}
				</ul>
			{/if}
			<div class="add">
				<label>
					<span class="lbl">Name</span>
					<select bind:value={draftName}>
						<option value="">— artifact name —</option>
						{#each artifactNames as n}
							<option value={n}>{n}</option>
						{/each}
					</select>
				</label>
				<label>
					<span class="lbl">Version</span>
					<select bind:value={draftVersion} disabled={!draftName}>
						<option value="">— version —</option>
						{#each versionsForName as v}
							<option value={v}>{v}</option>
						{/each}
					</select>
				</label>
				<button class="btn secondary" type="button" onclick={addRow}>Add</button>
			</div>
			<div class="actions">
				<button class="btn" disabled={busy} onclick={save}>Save</button>
				<button
					class="btn secondary"
					disabled={busy}
					onclick={() => {
						editing = false;
						error = '';
					}}
				>
					Cancel
				</button>
			</div>
		</div>
	{:else if files.length === 0}
		<p class="muted" style="margin-top:0.75rem">No shared files. Register an artifact, then Edit.</p>
	{:else}
		<div class="grid" style="margin-top:0.75rem">
			{#each files as f (f.name)}
				<div class="panel row">
					<div class="top">
						<strong class="mono">{f.name}</strong>
						{#if f.converged}
							<span class="pill ok">converged</span>
						{:else}
							<span class="pill lag">diverging</span>
						{/if}
					</div>
					<div class="vs muted mono tiny">
						<span>desired {f.desiredVersion} · {truncateDigest(f.desiredDigest)}</span>
						<span>running {truncateDigest(f.runningDigest) || '—'}</span>
					</div>
					{#if f.lastError}
						<p class="err mono">{f.lastError}</p>
					{/if}
				</div>
			{/each}
		</div>
	{/if}
</section>

<style>
	.shared {
		margin-top: 1.75rem;
	}
	.head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
	}
	.head h2 {
		margin: 0;
	}
	.grid {
		display: grid;
		gap: 0.75rem;
	}
	.row .top {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.5rem;
	}
	.vs {
		display: flex;
		flex-direction: column;
		gap: 0.15rem;
		margin-top: 0.35rem;
	}
	.err {
		color: var(--bad, #b33);
		margin: 0.35rem 0 0;
		font-size: 0.85rem;
	}
	.list {
		list-style: none;
		padding: 0;
		margin: 0 0 0.75rem;
	}
	.list li {
		display: flex;
		align-items: center;
		gap: 0.5rem;
		flex-wrap: wrap;
		margin-bottom: 0.4rem;
	}
	.add {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		align-items: flex-end;
		margin-bottom: 0.75rem;
	}
	.add label {
		display: flex;
		flex-direction: column;
		gap: 0.2rem;
	}
	.lbl {
		font-size: 0.75rem;
		color: var(--muted, #888);
	}
	.actions {
		display: flex;
		gap: 0.5rem;
	}
	.tiny {
		font-size: 0.8rem;
	}
</style>
