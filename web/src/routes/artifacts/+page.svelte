<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { ArtifactRef } from '$lib/gen/strategyplatform/v1/common_pb';
	import { ArtifactType } from '$lib/gen/strategyplatform/v1/common_pb';
	import {
		catalogFromList,
		groupCatalog,
		relativeTime,
		truncateDigest,
		typeLabel,
		type CatalogArtifact
	} from '$lib/artifacts';
	import {
		AUTO_HASH_MAX_BYTES,
		CryptoUnavailableError,
		FileTooLargeToHashError,
		sha256File,
		webCryptoSubtle
	} from '$lib/sha256';

	let artifacts = $state<CatalogArtifact[]>([]);
	let busy = $state(false);
	let error = $state('');
	let info = $state('');
	let expandedDigests = $state<Set<string>>(new Set());

	// Unified register form (page-top).
	let regName = $state('');
	let regVersion = $state('');
	let regDigest = $state('');
	let regUri = $state('');
	let regKind = $state<'binary' | 'config' | 'oci'>('binary');
	let regFile = $state<File | null>(null);
	let showRegister = $state(false);
	let hashing = $state(false);
	let digestLocked = $state(false);

	const groups = $derived(groupCatalog(artifacts));

	onMount(() => {
		void loadArtifacts();
		// Poll so PENDING → READY/FAILED updates without a manual refresh.
		const poll = setInterval(() => {
			void loadArtifacts();
		}, 2000);
		return () => clearInterval(poll);
	});

	async function loadArtifacts() {
		const res = await client.listArtifacts({});
		artifacts = catalogFromList(res);
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

	function openRegister(name = '', kind: 'binary' | 'config' | 'oci' = 'binary') {
		regName = name;
		regKind = kind;
		regVersion = '';
		regDigest = '';
		regUri = '';
		regFile = null;
		digestLocked = false;
		showRegister = true;
		error = '';
		info = '';
	}

	function artifactTypeOf(kind: 'binary' | 'config' | 'oci'): ArtifactType {
		return kind === 'oci' ? ArtifactType.OCI_IMAGE : ArtifactType.BINARY;
	}

	async function onFileChange(ev: Event) {
		const input = ev.currentTarget as HTMLInputElement;
		const file = input.files?.[0] ?? null;
		regFile = file;
		regDigest = '';
		digestLocked = false;
		if (!file) return;
		if (!webCryptoSubtle()) {
			error = new CryptoUnavailableError().message;
			info = '';
			return;
		}
		if (file.size > AUTO_HASH_MAX_BYTES) {
			error = '';
			info = new FileTooLargeToHashError(file.size).message;
			return;
		}
		hashing = true;
		error = '';
		info = '';
		try {
			regDigest = await sha256File(file);
			digestLocked = true;
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			digestLocked = false;
		} finally {
			hashing = false;
		}
	}

	async function register() {
		const name =
			regKind === 'config' && regName && !regName.endsWith('-config')
				? `${regName}-config`
				: regName;
		if (!name || !regVersion) {
			error = 'Name and version are required';
			return;
		}
		if (!regFile && (!regDigest || !regUri)) {
			error = 'Choose a file to upload, or provide digest + URI';
			return;
		}
		busy = true;
		error = '';
		info = '';
		try {
			let uri = regUri;
			let digest = regDigest;
			if (regFile) {
				if (!digest) {
					if (regFile.size > AUTO_HASH_MAX_BYTES || !webCryptoSubtle()) {
						throw new Error('Digest is required. Paste the sha256sum of the selected file.');
					}
					digest = await sha256File(regFile);
					regDigest = digest;
				}
				info = 'Requesting upload URL…';
				const up = await client.createArtifactUpload({
					name,
					version: regVersion,
					digest,
					type: artifactTypeOf(regKind)
				});
				info = `Uploading ${regFile.name}…`;
				const put = await fetch(up.putUrl, {
					method: 'PUT',
					body: regFile,
					headers: { 'Content-Type': 'application/octet-stream' }
				});
				if (!put.ok) {
					throw new Error(
						`upload failed (${put.status}). If this is a CORS error, PUT from curl — see README.`
					);
				}
				uri = up.s3Uri;
			}
			await client.registerArtifact({
				artifact: {
					name,
					version: regVersion,
					digest,
					uri,
					type: artifactTypeOf(regKind)
				}
			});
			info = `Registered ${name}@${regVersion}`;
			showRegister = false;
			await loadArtifacts();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
			info = '';
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
			<h2>Register {regKind === 'config' ? 'config' : regKind === 'oci' ? 'OCI image' : 'binary'}</h2>
			<p class="muted" style="margin-bottom:0.85rem">
				{#if regKind === 'config'}
					Stored as <span class="mono">&lt;name&gt;-config</span> when name has no
					<span class="mono">-config</span> suffix.
				{:else if regKind === 'oci'}
					Upload a <span class="mono">docker save</span> / OCI-layout tar, or register an existing URI.
					Large tars skip in-browser hashing — paste <span class="mono">sha256sum</span>.
				{:else}
					Upload a file (presigned PUT to the object store) or register an existing URI.
					Files over 64 MiB skip in-browser hashing — paste <span class="mono">sha256sum</span>.
				{/if}
			</p>
			<div class="form">
				<label>
					Kind
					<select bind:value={regKind}>
						<option value="binary">binary</option>
						<option value="config">config</option>
						<option value="oci">oci image</option>
					</select>
				</label>
				<label>
					Name
					<input bind:value={regName} placeholder={regKind === 'config' ? 'mystrat' : 'mystrat'} />
				</label>
				<label>Version<input bind:value={regVersion} placeholder="v42" /></label>
				<label>
					File
					<input type="file" disabled={busy || hashing} onchange={onFileChange} />
				</label>
				<label>Digest<input class="wide" bind:value={regDigest} placeholder={hashing ? 'hashing…' : 'sha256:…'} disabled={digestLocked} /></label>
				<label>URI<input class="wide" bind:value={regUri} placeholder={regFile ? 'filled after upload' : 's3://… or file:///…'} disabled={!!regFile} /></label>
				<button class="btn" disabled={busy || hashing} onclick={register}>
					{regFile ? 'Upload & register' : 'Register'}
				</button>
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
			<p class="muted">No artifacts yet. Register a binary, OCI image, or config to get started.</p>
		</div>
	{:else}
		<div class="groups" style="margin-top:1.25rem">
			{#each groups as g (g.name)}
				{@const latest = g.versions[0]}
				<div class="panel group">
					<div class="group-head">
						<div class="group-title">
							<span class="name mono">{g.name}</span>
							<span class="kind muted">({typeLabel(latest.ref, g.kind)})</span>
						</div>
						<button
							class="btn secondary"
							type="button"
							onclick={() =>
								openRegister(
									g.kind === 'config' ? g.name.replace(/-config$/, '') : g.name,
									g.kind === 'config' ? 'config' : g.kind === 'oci' ? 'oci' : 'binary'
								)}
						>
							Register new version
						</button>
					</div>
					<ul class="versions">
						{#each g.versions as row, i}
							{@const a = row.ref}
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
								{#if row.state === 'PENDING'}
									<span class="pill pending" title="Ingesting into object store…">
										<span class="spin" aria-hidden="true"></span>
										PENDING
									</span>
								{:else if row.state === 'FAILED'}
									<span class="pill bad" title={row.stateReason || 'ingest failed'}>FAILED</span>
								{:else if row.state === 'READY'}
									<span class="pill muted-pill" title="Ready to deploy">READY</span>
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
	.pill.pending {
		display: inline-flex;
		align-items: center;
		gap: 0.35rem;
		background: rgba(180, 140, 40, 0.14);
		color: #8a6a12;
	}
	.pill.muted-pill {
		background: rgba(0, 0, 0, 0.05);
		color: var(--ink-muted);
	}
	.spin {
		width: 0.65rem;
		height: 0.65rem;
		border: 1.5px solid currentColor;
		border-right-color: transparent;
		border-radius: 50%;
		animation: art-spin 0.7s linear infinite;
	}
	@keyframes art-spin {
		to {
			transform: rotate(360deg);
		}
	}
</style>
