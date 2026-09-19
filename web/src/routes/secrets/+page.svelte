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

	async function copyToken(token: string) {
		try {
			await navigator.clipboard.writeText(token);
			info = `Copied ${token}`;
		} catch {
			info = token;
		}
	}

	function fillName(n: string) {
		name = n;
		overwrite = true;
	}
</script>

<section class="fade-in">
	<div class="head">
		<div>
			<h1>Secrets</h1>
			<p class="muted">
				Named credentials for assignment env and <span class="mono">member.vars</span>. The public
				token is <span class="mono">secret.&lt;name&gt;</span>. Plaintext never leaves this form
				after submit — list shows name, length, and wrap key id only.
			</p>
		</div>
	</div>

	{#if dark}
		<p class="pill bad" style="margin-top:1rem">
			Secret management is down. Set <span class="mono">STRATEGON_SEAL_KEY</span> on the control
			plane. This page stays here so a missing key is not mistaken for an empty catalog.
		</p>
	{/if}

	<div class="panel" style="margin-top:1.25rem">
		<h2>{overwrite ? 'Overwrite' : 'Put'} secret</h2>
		<p class="muted" style="margin-bottom:0.85rem">
			Value is a password field and is cleared after submit. The browser does not store it.
		</p>
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
			<button class="btn" type="button" disabled={dark || busy || !name.trim()} onclick={onPut}>
				{overwrite ? 'Overwrite' : 'Put'}
			</button>
		</div>
		{#if lastToken}
			<p class="mono created" style="margin-top:0.85rem">
				Public token
				<button type="button" class="btn ghost" onclick={() => copyToken(lastToken)}>Copy</button>
				<br />
				<code>{lastToken}</code>
			</p>
		{/if}
	</div>

	{#if info}
		<p class="pill ok" style="margin-top:1rem">{info}</p>
	{/if}
	{#if error}
		<p class="pill bad" style="margin-top:1rem">{error}</p>
	{/if}

	{#if !dark && secrets.length === 0}
		<div class="panel empty" style="margin-top:1.25rem">
			<p class="muted">No secrets yet. Put one above, then paste <span class="mono">secret.&lt;name&gt;</span> into Deploy env.</p>
		</div>
	{:else if !dark}
		<table style="margin-top:1.25rem">
			<thead>
				<tr>
					<th>Name</th>
					<th>Token</th>
					<th>Bytes</th>
					<th>Key id</th>
					<th></th>
				</tr>
			</thead>
			<tbody>
				{#each secrets as s}
					<tr>
						<td class="mono">{s.name}</td>
						<td class="mono">{s.token}</td>
						<td>{s.lengthBytes}</td>
						<td class="mono">{s.keyId}</td>
						<td>
							<button type="button" class="btn ghost" onclick={() => copyToken(s.token)}>Copy</button>
							<button type="button" class="btn ghost" onclick={() => fillName(s.name)}>Overwrite</button>
						</td>
					</tr>
				{/each}
			</tbody>
		</table>
	{/if}
</section>

<style>
	.head {
		display: flex;
		align-items: flex-start;
		justify-content: space-between;
		gap: 1rem;
	}
	.form {
		display: flex;
		flex-wrap: wrap;
		gap: 0.75rem;
		align-items: flex-end;
	}
	.form label.wide {
		flex: 1 1 16rem;
	}
	.created {
		display: flex;
		flex-wrap: wrap;
		align-items: center;
		gap: 0.5rem;
	}
</style>
