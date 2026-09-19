<script lang="ts">
	import { onMount } from 'svelte';
	import { client } from '$lib/api';
	import type { SecretView } from '$lib/gen/strategyplatform/v1/control_service_pb';

	let secrets = $state<SecretView[]>([]);
	let error = $state('');
	let info = $state('');
	let dark = $state(false);
	let name = $state('');
	let value = $state('');
	let busy = $state(false);
	let lastToken = $state('');
	let overwrite = $state(false);
	let showPut = $state(false);

	onMount(() => {
		void load();
	});

	function isDarkError(e: unknown): boolean {
		const msg = e instanceof Error ? e.message : String(e);
		return /failed_precondition|unavailable/i.test(msg);
	}

	async function load() {
		error = '';
		try {
			const res = await client.listSecrets({});
			secrets = res.secrets;
			dark = false;
		} catch (e) {
			if (isDarkError(e)) {
				dark = true;
				secrets = [];
				return;
			}
			error = e instanceof Error ? e.message : String(e);
		}
	}

	function openPut(n = '') {
		name = n;
		value = '';
		overwrite = !!n && secrets.some((s) => s.name === n);
		showPut = true;
		error = '';
		info = '';
		lastToken = '';
	}

	async function onPut() {
		error = '';
		info = '';
		lastToken = '';
		const n = name.trim();
		if (!n) {
			error = 'Name is required';
			return;
		}
		if (overwrite) {
			const ok = confirm(
				'Overwrite this secret? Running processes keep the old env until the next start.'
			);
			if (!ok) return;
		}
		busy = true;
		try {
			const res = await client.putSecret({ name: n, value });
			lastToken = res.token;
			value = '';
			info = 'Stored. Copy the token — plaintext is not shown again.';
			await load();
		} catch (e) {
			if (isDarkError(e)) {
				dark = true;
				error = 'Secret management is down (STRATEGON_SEAL_KEY is not set).';
			} else {
				error = e instanceof Error ? e.message : String(e);
			}
		} finally {
			busy = false;
		}
	}

	async function onDelete(s: SecretView) {
		error = '';
		info = '';
		const ok = confirm(
			`Delete ${s.token}? Assignments that still reference it keep the token; the next resolve fails closed and running processes keep the old env.`
		);
		if (!ok) return;
		busy = true;
		try {
			await client.deleteSecret({ name: s.name });
			if (name.trim() === s.name) {
				name = '';
				overwrite = false;
			}
			if (lastToken === s.token) {
				lastToken = '';
			}
			info = `Deleted ${s.token}`;
			await load();
		} catch (e) {
			if (isDarkError(e)) {
				dark = true;
				error = 'Secret management is down (STRATEGON_SEAL_KEY is not set).';
			} else {
				error = e instanceof Error ? e.message : String(e);
			}
		} finally {
			busy = false;
		}
	}

	async function copyToken(token: string) {
		try {
			await navigator.clipboard.writeText(token);
			info = `Copied ${token}`;
		} catch {
			info = token;
		}
	}
</script>

<section class="fade-in page">
	<div class="head">
		<div class="head-copy">
			<h1>Secrets</h1>
			<p class="muted">
				Named credentials for assignment env and <span class="mono">member.vars</span>. The public
				token is <span class="mono">secret.&lt;name&gt;</span>. Plaintext never leaves this form
				after submit — list shows name, length, and wrap key id only.
			</p>
		</div>
		<button class="btn" type="button" disabled={dark} onclick={() => openPut()}>Put secret</button>
	</div>

	{#if dark}
		<p class="pill bad" style="margin-top:1rem">
			Secret management is down. Set <span class="mono">STRATEGON_SEAL_KEY</span> on the control
			plane. This page stays here so a missing key is not mistaken for an empty catalog.
		</p>
	{/if}

	{#if showPut}
		<div class="panel" style="margin-top:1.25rem">
			<div class="panel-head">
				<div>
					<h2>{overwrite ? 'Overwrite' : 'Put'} secret</h2>
					<p class="muted">
						Value is a password field and is cleared after submit. The browser does not store it.
					</p>
				</div>
				<button class="btn secondary" type="button" disabled={busy} onclick={() => (showPut = false)}>
					Cancel
				</button>
			</div>
			<div class="form">
				<label>
					Name
					<input
						bind:value={name}
						autocomplete="off"
						placeholder="db-url"
						disabled={dark || busy}
						oninput={() => (overwrite = secrets.some((s) => s.name === name.trim()))}
					/>
				</label>
				<label class="wide">
					Value
					<input
						type="password"
						bind:value={value}
						autocomplete="new-password"
						placeholder="plaintext — shown only in this field"
						disabled={dark || busy}
					/>
				</label>
				<button class="btn form-action" type="button" disabled={dark || busy || !name.trim()} onclick={onPut}>
					{overwrite ? 'Overwrite' : 'Put'}
				</button>
			</div>
			{#if lastToken}
				<div class="created">
					<code class="mono">{lastToken}</code>
					<button type="button" class="btn secondary" onclick={() => copyToken(lastToken)}>Copy</button>
				</div>
			{/if}
		</div>
	{/if}

	{#if info}
		<p class="pill ok" style="margin-top:1rem">{info}</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}

	{#if !dark && secrets.length === 0}
		<div class="panel empty" style="margin-top:1.25rem">
			<p class="muted">
				No secrets yet. Put one, then paste <span class="mono">secret.&lt;name&gt;</span> into Deploy
				env.
			</p>
		</div>
	{:else if !dark}
		<div class="table-wrap">
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Token</th>
						<th>Bytes</th>
						<th>Key</th>
						<th class="actions">Actions</th>
					</tr>
				</thead>
				<tbody>
					{#each secrets as s (s.name)}
						<tr>
							<td class="mono name">{s.name}</td>
							<td class="mono token">{s.token}</td>
							<td>{s.lengthBytes}</td>
							<td class="mono">{s.keyId || '—'}</td>
							<td class="actions">
								<button type="button" class="btn secondary" disabled={busy} onclick={() => copyToken(s.token)}>
									Copy
								</button>
								<button type="button" class="btn secondary" disabled={busy} onclick={() => openPut(s.name)}>
									Overwrite
								</button>
								<button type="button" class="btn danger" disabled={busy} onclick={() => onDelete(s)}>
									Delete
								</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>
	{/if}
</section>

<style>
	.page {
		width: 100%;
	}
	.head,
	.panel-head,
	.created,
	.form {
		display: flex;
		align-items: flex-end;
		justify-content: space-between;
		gap: 1rem;
		width: 100%;
	}
	.head,
	.panel-head {
		align-items: flex-start;
	}
	.head-copy {
		text-align: left;
		min-width: 0;
		flex: 1;
	}
	.head > .btn,
	.panel-head > .btn,
	.form-action,
	.created > .btn {
		margin-left: auto;
		flex-shrink: 0;
	}
	.form {
		flex-wrap: wrap;
		align-items: flex-end;
		margin-top: 0.85rem;
	}
	.form label {
		text-align: left;
	}
	.form label.wide {
		flex: 1 1 16rem;
	}
	.empty {
		text-align: left;
	}
	.table-wrap {
		width: 100%;
		margin-top: 1.25rem;
		overflow-x: auto;
		border: 1px solid var(--line);
		border-radius: var(--radius);
		background: var(--surface);
		box-shadow: var(--shadow);
	}
	table {
		width: 100%;
		border-collapse: collapse;
		table-layout: auto;
		font-size: 0.9rem;
	}
	th,
	td {
		text-align: left;
		padding: 0.7rem 0.85rem;
		border-bottom: 1px solid var(--line);
		vertical-align: middle;
	}
	th {
		font-size: 0.72rem;
		font-weight: 700;
		text-transform: uppercase;
		letter-spacing: 0.04em;
		color: var(--ink-muted);
		background: rgba(255, 255, 255, 0.55);
	}
	tbody tr:last-child td {
		border-bottom: none;
	}
	.name {
		font-weight: 650;
		color: var(--ink);
	}
	.token {
		color: var(--ink-muted);
	}
	th.actions,
	td.actions {
		text-align: right;
	}
	td.actions {
		white-space: nowrap;
	}
	td.actions .btn {
		margin-left: 0.35rem;
	}
	.created {
		margin-top: 0.85rem;
		align-items: center;
	}
	.created code {
		word-break: break-all;
	}
</style>
