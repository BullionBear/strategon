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
	let configVersion = $state('');
	let argsText = $state('-c ${CONFIG}');
	let envText = $state('');
	let regName = $state('');
	let regVersion = $state('');
	let regDigest = $state('');
	let regUri = $state('');
	let cfgVersion = $state('');
	let cfgDigest = $state('');
	let cfgUri = $state('');
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

	async function registerBinary() {
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
			info = `Registered binary ${regName}@${regVersion}`;
			if (!strategy) strategy = regName;
			if (!version) version = regVersion;
			await loadArtifacts();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	async function registerConfig() {
		const name = strategy || regName;
		if (!name) {
			error = 'Set strategy (or binary name) first — config is registered as <strategy>-config';
			return;
		}
		busy = true;
		error = '';
		info = '';
		try {
			const configName = `${name}-config`;
			await client.registerArtifact({
				artifact: {
					name: configName,
					version: cfgVersion,
					digest: cfgDigest,
					uri: cfgUri,
					type: ArtifactType.BINARY
				}
			});
			info = `Registered config ${configName}@${cfgVersion}`;
			if (!configVersion) configVersion = cfgVersion;
			await loadArtifacts();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	function parseArgs(text: string): string[] {
		const trimmed = text.trim();
		if (!trimmed) return [];
		// Simple whitespace split; quote-aware enough for typical "-c ${CONFIG}".
		const out: string[] = [];
		const re = /"([^"]*)"|'([^']*)'|(\S+)/g;
		let m: RegExpExecArray | null;
		while ((m = re.exec(trimmed)) !== null) {
			out.push(m[1] ?? m[2] ?? m[3] ?? '');
		}
		return out;
	}

	function parseEnv(text: string): Record<string, string> {
		const env: Record<string, string> = {};
		for (const line of text.split(/\r?\n/)) {
			const t = line.trim();
			if (!t || t.startsWith('#')) continue;
			const eq = t.indexOf('=');
			if (eq <= 0) continue;
			env[t.slice(0, eq)] = t.slice(eq + 1);
		}
		return env;
	}

	async function setDeployment() {
		busy = true;
		error = '';
		info = '';
		try {
			const res = await client.setDeployment({
				machineId,
				strategy,
				artifactVersion: version,
				configVersion,
				args: parseArgs(argsText),
				env: parseEnv(envText)
			});
			info = `Deployment accepted — generation ${res.generation}. Watching convergence…`;
			await goto(`/machines/${machineId}/${strategy}`);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}

	const binaryVersions = $derived(
		artifacts.filter((a) => a.name === strategy || a.name === regName).map((a) => a.version)
	);
	const configVersions = $derived(
		artifacts
			.filter((a) => a.name === `${strategy}-config` || a.name === `${regName}-config`)
			.map((a) => a.version)
	);
</script>

<section class="fade-in">
	<h1>Deploy</h1>
	<p class="muted">
		Register immutable artifacts, then set a full deployment (binary + config + args + env).
		Success is when observed generation catches up — not when this form returns.
	</p>

	<div class="panel" style="margin-top:1.25rem">
		<h2>1. Register binary</h2>
		<p class="muted" style="margin-bottom:0.85rem">
			Digest + URI are required so agents can fetch and verify.
		</p>
		<div class="form">
			<label>Name<input bind:value={regName} placeholder="strategy name" /></label>
			<label>Version<input bind:value={regVersion} placeholder="v42" /></label>
			<label>Digest<input class="wide" bind:value={regDigest} placeholder="sha256:…" /></label>
			<label>URI<input class="wide" bind:value={regUri} placeholder="file:///path/to/bin" /></label>
			<button class="btn secondary" disabled={busy} onclick={registerBinary}>Register binary</button>
		</div>
	</div>

	<div class="panel" style="margin-top:1rem">
		<h2>2. Register config</h2>
		<p class="muted" style="margin-bottom:0.85rem">
			Stored as <span class="mono">&lt;strategy&gt;-config</span> in the same artifact registry.
			Keep the original extension in the URI (e.g. <span class="mono">config.yml</span>).
		</p>
		<div class="form">
			<label>Version<input bind:value={cfgVersion} placeholder="c17" /></label>
			<label>Digest<input class="wide" bind:value={cfgDigest} placeholder="sha256:…" /></label>
			<label>URI<input class="wide" bind:value={cfgUri} placeholder="file:///path/to/config.yml" /></label>
			<button class="btn secondary" disabled={busy || !(strategy || regName)} onclick={registerConfig}>
				Register config
			</button>
		</div>
	</div>

	<div class="panel" style="margin-top:1rem">
		<h2>3. Set deployment</h2>
		<p class="muted" style="margin-bottom:0.85rem">
			Args may use <span class="mono">${'{CONFIG}'}</span>,
			<span class="mono">${'{RELEASE_DIR}'}</span>,
			<span class="mono">${'{BINARY}'}</span> — resolved by the agent against
			<span class="mono">current/</span>.
		</p>
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
			<label>Strategy<input bind:value={strategy} placeholder="mystrat" /></label>
			<label>
				Binary version
				{#if binaryVersions.length}
					<select bind:value={version}>
						<option value="">Select…</option>
						{#each binaryVersions as v}
							<option value={v}>{v}</option>
						{/each}
					</select>
				{:else}
					<input bind:value={version} placeholder="v42" />
				{/if}
			</label>
			<label>
				Config version
				{#if configVersions.length}
					<select bind:value={configVersion}>
						<option value="">(keep / none)</option>
						{#each configVersions as v}
							<option value={v}>{v}</option>
						{/each}
					</select>
				{:else}
					<input bind:value={configVersion} placeholder="c17 (optional)" />
				{/if}
			</label>
			<label class="block">
				Args
				<input class="wide" bind:value={argsText} placeholder={'-c ${CONFIG}'} />
			</label>
			<label class="block">
				Env <span class="muted">(KEY=value, one per line)</span>
				<textarea bind:value={envText} rows="3" placeholder={"FOO=bar\nBAZ=1"}></textarea>
			</label>
			<button
				class="btn"
				disabled={busy || !machineId || !strategy || !version}
				onclick={setDeployment}
			>
				Set deployment
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
	.form :global(input.wide),
	.form textarea {
		min-width: 18rem;
	}
	.form label.block {
		flex: 1 1 100%;
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
		align-items: stretch;
	}
	.form textarea {
		font: inherit;
		font-family: var(--mono, ui-monospace, monospace);
		padding: 0.45rem 0.6rem;
		border: 1px solid var(--line, #ddd);
		border-radius: 6px;
		background: var(--surface, #fff);
		resize: vertical;
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
