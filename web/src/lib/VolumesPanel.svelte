<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { VolumeView } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { formatBytes } from '$lib/fleet';

	interface Props {
		machineId: string;
	}
	let { machineId }: Props = $props();

	let volumes = $state<VolumeView[]>([]);
	let error = $state('');
	let busy = $state(false);
	let draftName = $state('');

	async function load() {
		if (!machineId) return;
		try {
			const res = await client.listVolumes({ machineId });
			volumes = res.volumes;
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

	async function create() {
		const name = draftName.trim();
		if (!name) {
			error = 'Volume name is required';
			return;
		}
		busy = true;
		error = '';
		try {
			await client.createVolume({ machineId, name });
			draftName = '';
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function remove(name: string) {
		busy = true;
		error = '';
		try {
			await client.deleteVolume({ machineId, name });
			await load();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="volumes">
	<div class="head">
		<h2>Volumes</h2>
	</div>
	<p class="muted">
		Machine-level named directories under <span class="mono">./volumes/</span>. Assignments mount
		by name; delete is blocked while any assignment (including stopped) pins the name.
	</p>

	{#if error}
		<p class="pill bad">{error}</p>
	{/if}

	<div class="add">
		<label>
			<span class="lbl">Name</span>
			<input class="mono" bind:value={draftName} placeholder="mftik-data" />
		</label>
		<button class="btn secondary" type="button" disabled={busy} onclick={create}>Create</button>
	</div>

	{#if volumes.length === 0}
		<p class="muted" style="margin-top:0.75rem">No volumes. Create one, then mount it from apply YAML.</p>
	{:else}
		<div class="grid" style="margin-top:0.75rem">
			{#each volumes as v (v.name)}
				<div class="panel row">
					<div class="top">
						<a class="mono" href="/machines/{machineId}/volumes/{encodeURIComponent(v.name)}">
							<strong>{v.name}</strong>
						</a>
						{#if v.ready}
							<span class="pill ok">ready</span>
						{:else}
							<span class="pill lag">pending</span>
						{/if}
					</div>
					<div class="vs muted mono tiny">
						<span>size {v.sizeBytes ? formatBytes(v.sizeBytes) : '—'}</span>
						<span>
							mounted by {v.mountedBy.length ? v.mountedBy.join(', ') : '—'}
						</span>
						<span>
							pinned by {v.pinnedBy.length ? v.pinnedBy.join(', ') : '—'}
						</span>
					</div>
					{#if v.lastError}
						<p class="err mono">{v.lastError}</p>
					{/if}
					<button
						class="btn danger"
						type="button"
						disabled={busy || v.pinnedBy.length > 0}
						onclick={() => remove(v.name)}
					>
						Delete
					</button>
				</div>
			{/each}
		</div>
	{/if}
</section>

<style>
	.volumes {
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
	.add {
		display: flex;
		flex-wrap: wrap;
		gap: 0.5rem;
		align-items: flex-end;
		margin-top: 0.75rem;
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
	.tiny {
		font-size: 0.8rem;
	}
</style>
