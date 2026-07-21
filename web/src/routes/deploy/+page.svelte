<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { page } from '$app/state';
	import { client } from '$lib/api';
	import type { ArtifactRef } from '$lib/gen/strategyplatform/v1/common_pb';
	import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';
	import { groupArtifacts, latestVersion, versionsFor } from '$lib/artifacts';

	let machines = $state<Machine[]>([]);
	let artifacts = $state<ArtifactRef[]>([]);
	let machineId = $state('');
	let strategy = $state('');
	let version = $state('');
	let configVersion = $state('');
	let argsText = $state('-c ${CONFIG}');
	let envText = $state('');
	let busy = $state(false);
	let error = $state('');
	let info = $state('');
	let versionTouched = $state(false);
	let configTouched = $state(false);
	let strategyOpen = $state(false);

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

	/** Distinct binary artifact names (exclude `-config` siblings). */
	const strategyOptions = $derived(
		groupArtifacts(artifacts)
			.filter((g) => g.kind === 'binary')
			.map((g) => g.name)
	);

	const strategyFiltered = $derived.by(() => {
		const q = strategy.trim().toLowerCase();
		if (!q) return strategyOptions;
		return strategyOptions.filter((n) => n.toLowerCase().includes(q));
	});

	const strategyExact = $derived(
		strategyOptions.some((n) => n.toLowerCase() === strategy.trim().toLowerCase())
	);

	const binaryOptions = $derived(strategy ? versionsFor(artifacts, strategy) : []);
	const configOptions = $derived(strategy ? versionsFor(artifacts, `${strategy}-config`) : []);
	const hasBinary = $derived(binaryOptions.length > 0);

	// Default version selectors to latest when strategy changes (unless user picked).
	$effect(() => {
		const s = strategy;
		if (!s) return;
		if (!versionTouched) {
			version = latestVersion(artifacts, s) ?? '';
		}
		if (!configTouched) {
			configVersion = latestVersion(artifacts, `${s}-config`) ?? '';
		}
	});

	function selectStrategy(name: string) {
		strategy = name;
		versionTouched = false;
		configTouched = false;
		strategyOpen = false;
	}

	function onStrategyInput() {
		versionTouched = false;
		configTouched = false;
		strategyOpen = true;
	}

	function onStrategyBlur() {
		// Delay so mousedown on an option beats blur.
		setTimeout(() => {
			strategyOpen = false;
		}, 150);
	}

	function parseArgs(text: string): string[] {
		const trimmed = text.trim();
		if (!trimmed) return [];
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
			info = `Deployment created (stopped) — generation ${res.generation}. Start it to run.`;
			await goto(`/machines/${machineId}/${strategy}`);
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		} finally {
			busy = false;
		}
	}
</script>

<section class="fade-in">
	<h1>Deploy</h1>
	<p class="muted">
		Create a deployment (binary + config + args + env). New deployments land
		<strong>stopped</strong> — use Start on the strategy page to run them. Versions
		default to the newest registered artifact; the deployment pins that concrete version.
		<a href="/artifacts">Manage catalog →</a>
	</p>

	<div class="panel" style="margin-top:1.25rem">
		<div class="form">
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
			<label class="strategy-field">
				Strategy
				<div class="combo">
					<input
						bind:value={strategy}
						placeholder="mystrat"
						autocomplete="off"
						role="combobox"
						aria-expanded={strategyOpen}
						aria-controls="strategy-list"
						aria-autocomplete="list"
						oninput={onStrategyInput}
						onfocus={() => (strategyOpen = true)}
						onblur={onStrategyBlur}
					/>
					{#if strategyOpen}
						<ul id="strategy-list" class="combo-list" role="listbox">
							{#each strategyFiltered as name}
								<li role="option" aria-selected={strategy === name}>
									<button
										type="button"
										class="combo-opt"
										onmousedown={(e) => {
											e.preventDefault();
											selectStrategy(name);
										}}
									>
										{name}
									</button>
								</li>
							{:else}
								<li class="combo-empty muted">
									{#if strategy.trim() && !strategyExact}
										<span class="mono">{strategy.trim()}</span> will be created as a new
										strategy
									{:else}
										No known strategies yet — type a name to create one
									{/if}
								</li>
							{/each}
						</ul>
					{/if}
				</div>
			</label>
			<label>
				Binary
				{#if binaryOptions.length}
					<select
						bind:value={version}
						onchange={() => (versionTouched = true)}
						title="當前最新註冊版本；部署會釘死此版本"
					>
						{#each binaryOptions as opt}
							<option value={opt.version}>
								{opt.version}{opt.latest ? ' (latest)' : ''}
							</option>
						{/each}
					</select>
				{:else if strategy}
					<span class="empty-hint muted">
						No binary registered.
						<a href="/artifacts">Register on Artifacts →</a>
					</span>
				{:else}
					<span class="empty-hint muted">Enter a strategy name first</span>
				{/if}
			</label>
			<label>
				Config
				{#if configOptions.length}
					<select bind:value={configVersion} onchange={() => (configTouched = true)}>
						<option value="">none</option>
						{#each configOptions as opt}
							<option value={opt.version}>
								{opt.version}{opt.latest ? ' (latest)' : ''}
							</option>
						{/each}
					</select>
				{:else}
					<span class="empty-hint muted">
						none
						{#if strategy}
							· <a href="/artifacts">register config →</a>
						{/if}
					</span>
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
				disabled={busy || !machineId || !strategy || !version || !hasBinary}
				onclick={setDeployment}
			>
				Deploy
			</button>
		</div>
		<p class="muted hint">
			Args may use <span class="mono">${'{CONFIG}'}</span>,
			<span class="mono">${'{RELEASE_DIR}'}</span>,
			<span class="mono">${'{BINARY}'}</span> — resolved by the agent against
			<span class="mono">current/</span>.
		</p>
	</div>

	{#if info}
		<p class="pill ok" style="margin-top:1rem">{info}</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
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
		min-width: min(18rem, 100%);
		max-width: 100%;
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
	.strategy-field {
		position: relative;
		min-width: 12rem;
	}
	.combo {
		position: relative;
	}
	.combo-list {
		position: absolute;
		z-index: 20;
		left: 0;
		right: 0;
		top: calc(100% + 2px);
		margin: 0;
		padding: 0.25rem 0;
		list-style: none;
		max-height: 14rem;
		overflow-y: auto;
		background: var(--surface, #fff);
		border: 1px solid var(--line, #ddd);
		border-radius: 6px;
		box-shadow: var(--shadow, 0 4px 16px rgba(0, 0, 0, 0.08));
	}
	.combo-opt {
		display: block;
		width: 100%;
		text-align: left;
		appearance: none;
		border: 0;
		background: transparent;
		padding: 0.45rem 0.7rem;
		font: inherit;
		font-family: var(--mono, ui-monospace, monospace);
		cursor: pointer;
		color: inherit;
	}
	.combo-opt:hover {
		background: rgba(13, 115, 119, 0.08);
	}
	.combo-empty {
		padding: 0.55rem 0.7rem;
		font-size: 0.85rem;
		font-weight: 400;
	}
	.empty-hint {
		display: inline-block;
		padding: 0.5rem 0;
		font-size: 0.9rem;
		font-weight: 400;
		min-width: 12rem;
	}
	.hint {
		margin: 1rem 0 0;
		font-size: 0.85rem;
	}
</style>
