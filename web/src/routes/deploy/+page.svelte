<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { client } from '$lib/api';
	import type { ArtifactRef } from '$lib/gen/strategyplatform/v1/common_pb';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { ArtifactType } from '$lib/gen/strategyplatform/v1/common_pb';

	let machines = $state<Machine[]>([]);
	let artifacts = $state<ArtifactRef[]>([]);
	let machineId = $state('');
	let strategy = $state('');
	let version = $state('');
	let regName = $state('');
	let regVersion = $state('');
	let regDigest = $state('');
	let regUri = $state('');
	let busy = $state(false);
	let error = $state('');
	let info = $state('');

	onMount(async () => {
		machineId = page.url.searchParams.get('machine') || '';
		strategy = page.url.searchParams.get('strategy') || '';
		await Promise.all([loadMachines(), loadArtifacts()]);
	});

	async function loadMachines() {
		const res = await client.listMachines({});
		machines = res.machines;
		if (!machineId && machines[0]?.metadata?.uid) {
			machineId = machines[0].metadata.uid;
		}
	}

	async function loadArtifacts() {
		const res = await client.listArtifacts({});
		artifacts = res.artifacts;
	}

	async function registerArtifact() {
		busy = true;
		error = '';
		info = '';
		try {
			await client.registerArtifact({
				artifact: {
					name: regName,
					version: regVersion,
					digest: regDigest,
					uri: regUri,
					type: ArtifactType.BINARY
				}
			});
			info = `Registered ${regName}@${regVersion}`;
			if (!strategy) strategy = regName;
			if (!version) version = regVersion;
			await loadArtifacts();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function deploy() {
		busy = true;
		error = '';
		info = '';
		try {
			const res = await client.deploy({
				machineId,
				strategy,
				artifactVersion: version,
				configVersion: ''
			});
			info = `Deploy accepted — generation ${res.generation}. Watching convergence…`;
			await goto(`/machines/${machineId}/${strategy}`);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	const versionsForStrategy = $derived(
		artifacts.filter((a) => a.name === strategy || a.name === regName).map((a) => a.version)
	);
</script>

<section class="fade-in">
	<h1>Deploy</h1>
	<p class="muted">
		Register an artifact, then ask the control plane to update desired state. Success is when
		observed generation catches up — not when this form returns.
	</p>

	<div class="panel" style="margin-top:1.25rem">
		<h2>1. Register artifact</h2>
		<p class="muted" style="margin-bottom:0.85rem">
			Digest + URI are required so agents can fetch and verify.
		</p>
		<div class="form">
			<label>Name<input bind:value={regName} placeholder="strategy name" /></label>
			<label>Version<input bind:value={regVersion} placeholder="v42" /></label>
			<label>Digest<input class="wide" bind:value={regDigest} placeholder="sha256:…" /></label>
			<label>URI<input class="wide" bind:value={regUri} placeholder="file:///path/to/bin" /></label>
			<button class="btn secondary" disabled={busy} onclick={registerArtifact}>Register</button>
		</div>
	</div>

	<div class="panel" style="margin-top:1rem">
		<h2>2. Deploy</h2>
		<div class="form" style="margin-top:0.85rem">
			<label>
				Machine
				<select bind:value={machineId}>
					<option value="">Select…</option>
					{#each machines as m}
						{@const mid = m.metadata?.uid || m.metadata?.name || ''}
						<option value={mid}>{mid}</option>
					{/each}
				</select>
			</label>
			<label>Strategy<input bind:value={strategy} placeholder="s" /></label>
			<label>
				Version
				{#if versionsForStrategy.length}
					<select bind:value={version}>
						<option value="">Select…</option>
						{#each versionsForStrategy as v}
							<option value={v}>{v}</option>
						{/each}
					</select>
				{:else}
					<input bind:value={version} placeholder="v42" />
				{/if}
			</label>
			<button class="btn" disabled={busy || !machineId || !strategy || !version} onclick={deploy}>
				Deploy
			</button>
		</div>
	</div>

	{#if info}
		<p class="pill ok" style="margin-top:1rem">{info}</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}

	{#if artifacts.length}
		<div style="margin-top:1.5rem">
			<h2>Catalog</h2>
			<ul class="catalog">
				{#each artifacts as a}
					<li class="mono">{a.name}@{a.version} · {a.digest}</li>
				{/each}
			</ul>
		</div>
	{/if}
</section>

<style>
	.form {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		align-items: flex-end;
	}
	.form :global(input.wide) {
		min-width: 18rem;
	}
	.catalog {
		list-style: none;
		padding: 0;
		margin: 0.5rem 0 0;
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
		font-size: 0.85rem;
		color: var(--ink-muted);
	}
</style>
