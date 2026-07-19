<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { ArtifactRef } from '$lib/gen/strategyplatform/v1/common_pb';
	import { ArtifactType } from '$lib/gen/strategyplatform/v1/common_pb';
	import { groupArtifacts, relativeTime, truncateDigest, typeLabel } from '$lib/artifacts';

	let artifacts = $state<ArtifactRef[]>([]);
	let busy = $state(false);
	let error = $state('');
	let info = $state('');
	let expandedDigests = $state<Set<string>>(new Set());

	// Unified register form (page-top).
	let regName = $state('');
	let regVersion = $state('');
	let regDigest = $state('');
	let regUri = $state('');
	let regKind = $state<'binary' | 'config'>('binary');
	let showRegister = $state(false);

	const groups = $derived(groupArtifacts(artifacts));

	onMount(async () => {
		await loadArtifacts();
	});

	async function loadArtifacts() {
		const res = await client.listArtifacts({});
		artifacts = res.artifacts;
	}

	function digestKey(a: ArtifactRef): string {
		return `${a.name}\0${a.version}`;
	}

	function toggleDigest(a: ArtifactRef) {
		const k = digestKey(a);
		const next = new Set(expandedDigests);
		if (next.has(k)) next.delete(k);
		else next.add(k);
		expandedDigests = next;
	}

	function openRegister(name = '', kind: 'binary' | 'config' = 'binary') {
		regName = name;
		regKind = kind;
		regVersion = '';
		regDigest = '';
		regUri = '';
		showRegister = true;
		error = '';
		info = '';
	}

	async function register() {
		const name =
			regKind === 'config' && regName && !regName.endsWith('-config')
				? `${regName}-config`
				: regName;
		if (!name || !regVersion || !regDigest || !regUri) {
			error = 'Name, version, digest, and URI are required';
			return;
		}
		busy = true;
		error = '';
		info = '';
		try {
			await client.registerArtifact({
				artifact: {
					name,
					version: regVersion,
					digest: regDigest,
					uri: regUri,
					type: ArtifactType.BINARY
				}
			});
			info = `Registered ${name}@${regVersion}`;
			showRegister = false;
			await loadArtifacts();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="fade-in">
	<div class="head">
		<div>
			<h1>Artifacts</h1>
			<p class="muted">
				Catalog grouped by name. <span class="pill ok" title="當前最新註冊版本；部署會釘死此版本"
					>latest</span
				>
				is the newest registration — deploy pins that concrete version, it does not follow latest.
			</p>
		</div>
		<button class="btn" type="button" onclick={() => openRegister()}>Register artifact</button>
	</div>

	{#if showRegister}
		<div class="panel register" style="margin-top:1.25rem">
			<h2>Register {regKind === 'config' ? 'config' : 'binary'}</h2>
			<p class="muted" style="margin-bottom:0.85rem">
				{#if regKind === 'config'}
					Stored as <span class="mono">&lt;name&gt;-config</span> when name has no
					<span class="mono">-config</span> suffix.
				{:else}
					Digest + URI are required so agents can fetch and verify.
				{/if}
			</p>
			<div class="form">
				<label>
					Kind
					<select bind:value={regKind}>
						<option value="binary">binary</option>
						<option value="config">config</option>
					</select>
				</label>
				<label>
					Name
					<input bind:value={regName} placeholder={regKind === 'config' ? 'mystrat' : 'mystrat'} />
				</label>
				<label>Version<input bind:value={regVersion} placeholder="v42" /></label>
				<label>Digest<input class="wide" bind:value={regDigest} placeholder="sha256:…" /></label>
				<label>URI<input class="wide" bind:value={regUri} placeholder="file:///path/to/bin" /></label>
				<button class="btn" disabled={busy} onclick={register}>Register</button>
				<button class="btn secondary" type="button" disabled={busy} onclick={() => (showRegister = false)}
					>Cancel</button
				>
			</div>
		</div>
	{/if}

	{#if info}
		<p class="pill ok" style="margin-top:1rem">{info}</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}

	{#if groups.length === 0}
		<div class="panel empty" style="margin-top:1.25rem">
			<p class="muted">No artifacts yet. Register a binary or config to get started.</p>
		</div>
	{:else}
		<div class="groups" style="margin-top:1.25rem">
			{#each groups as g (g.name)}
				{@const latest = g.versions[0]}
				<div class="panel group">
					<div class="group-head">
						<div class="group-title">
							<span class="name mono">{g.name}</span>
							<span class="kind muted">({typeLabel(latest, g.kind)})</span>
						</div>
						<button
							class="btn secondary"
							type="button"
							onclick={() =>
								openRegister(
									g.kind === 'config' ? g.name.replace(/-config$/, '') : g.name,
									g.kind === 'config' ? 'config' : 'binary'
								)}
						>
							Register new version
						</button>
					</div>
					<ul class="versions">
						{#each g.versions as a, i}
							{@const isLatest = i === 0}
							{@const key = digestKey(a)}
							{@const expanded = expandedDigests.has(key)}
							<li class:latest={isLatest}>
								<span class="dot" class:on={isLatest} aria-hidden="true"></span>
								<span class="ver mono">{a.version}</span>
								{#if isLatest}
									<span
										class="pill ok"
										title="當前最新註冊版本；部署會釘死此版本"
										>latest</span
									>
								{/if}
								<button
									type="button"
									class="digest mono"
									title={a.digest}
									onclick={() => toggleDigest(a)}
								>
									{expanded ? a.digest : truncateDigest(a.digest)}
								</button>
								<span class="when muted" title={a.createdAt ? new Date(Number(a.createdAt.seconds) * 1000).toISOString() : ''}
									>{relativeTime(a)}</span
								>
							</li>
						{/each}
					</ul>
				</div>
			{/each}
		</div>
	{/if}
</section>

<style>
	.head {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
		flex-wrap: wrap;
	}
	.form {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		align-items: flex-end;
	}
	.form :global(input.wide) {
		min-width: min(18rem, 100%);
		max-width: 100%;
	}
	.groups {
		display: flex;
		flex-direction: column;
		gap: 0.85rem;
	}
	.group-head {
		display: flex;
		align-items: center;
		justify-content: space-between;
		gap: 0.75rem;
		flex-wrap: wrap;
		margin-bottom: 0.65rem;
	}
	.group-title {
		display: flex;
		align-items: baseline;
		gap: 0.45rem;
	}
	.group-title .name {
		font-size: 1.05rem;
		font-weight: 600;
		color: var(--ink);
	}
	.versions {
		list-style: none;
		padding: 0;
		margin: 0;
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
	}
	.versions li {
		display: flex;
		align-items: center;
		flex-wrap: wrap;
		gap: 0.55rem 0.75rem;
		padding: 0.4rem 0.5rem;
		border-radius: 8px;
		font-size: 0.9rem;
	}
	.versions li.latest {
		background: rgba(13, 115, 119, 0.08);
	}
	.dot {
		width: 0.45rem;
		height: 0.45rem;
		border-radius: 50%;
		background: transparent;
		border: 1.5px solid var(--line);
		flex-shrink: 0;
	}
	.dot.on {
		background: var(--accent);
		border-color: var(--accent);
	}
	.ver {
		min-width: 3rem;
		font-weight: 600;
	}
	.digest {
		appearance: none;
		border: none;
		background: transparent;
		padding: 0;
		color: var(--ink-muted);
		cursor: pointer;
		text-align: left;
		max-width: 100%;
		overflow-wrap: anywhere;
	}
	.digest:hover {
		color: var(--accent-ink);
		text-decoration: underline;
	}
	.when {
		margin-left: auto;
		font-size: 0.82rem;
		white-space: nowrap;
	}
	.empty {
		text-align: center;
		padding: 2rem 1rem;
	}
</style>
