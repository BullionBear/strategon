<script lang="ts">
	import { onMount } from 'svelte';
	import {
		createAPIToken,
		fetchAuthStatus,
		listAPITokens,
		revokeAPIToken,
		type TokenMeta
	} from '$lib/auth';

	let tokens = $state<TokenMeta[]>([]);
	let error = $state('');
	let name = $state('ci');
	let created = $state('');
	let mode = $state('none');
	let signedIn = $state(false);

	async function reload() {
		const st = await fetchAuthStatus();
		mode = st.mode;
		signedIn = !!st.user || st.mode === 'none';
		if (!signedIn) {
			tokens = [];
			return;
		}
		tokens = await listAPITokens();
	}

	onMount(() => {
		reload().catch((e) => {
			error = e instanceof Error ? e.message : String(e);
		});
	});

	async function onCreate() {
		error = '';
		created = '';
		try {
			const res = await createAPIToken(name.trim() || 'default');
			created = res.token;
			await reload();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}

	async function onRevoke(id: string) {
		error = '';
		try {
			await revokeAPIToken(id);
			await reload();
		} catch (e) {
			error = e instanceof Error ? e.message : String(e);
		}
	}
</script>

<section class="fade-in">
	<h1>API tokens</h1>
	<p class="muted">
		Long-lived Bearer tokens for curl/CI. Issued to your Discord (or mock) identity; every
		mutating call is audited as you. Tokens are stored hashed in the control-plane process and
		are lost on restart unless durable auth storage is added later.
	</p>
	<p class="muted" style="margin-top:0.65rem">
		Connect JSON reference (standalone):
		<a href="/reference" target="_blank" rel="noopener noreferrer">Open API docs</a>
		·
		<a href="/openapi.json" target="_blank" rel="noopener noreferrer"><code class="mono">openapi.json</code></a>
	</p>

	{#if mode !== 'none' && !signedIn}
		<p class="pill bad" style="margin-top:1rem">Sign in first to mint tokens.</p>
	{:else}
		<div class="panel" style="margin-top:1.25rem">
			<label>
				Name
				<input bind:value={name} placeholder="ci" />
			</label>
			<button type="button" class="btn" style="margin-top:0.75rem" onclick={onCreate}>
				Create token
			</button>
			{#if created}
				<p class="mono created" style="margin-top:0.85rem">
					Copy now — shown once:<br />
					<code>{created}</code>
				</p>
			{/if}
		</div>

		{#if error}
			<p class="pill bad" style="margin-top:1rem">{error}</p>
		{/if}

		{#if tokens.length === 0}
			<p class="muted" style="margin-top:1.25rem">No tokens yet.</p>
		{:else}
			<table>
				<thead>
					<tr>
						<th>Name</th>
						<th>Id</th>
						<th>Created</th>
						<th>Last used</th>
						<th></th>
					</tr>
				</thead>
				<tbody>
					{#each tokens as t}
						<tr>
							<td>{t.name}</td>
							<td class="mono">{t.id}</td>
							<td class="mono">{t.created_at ? new Date(t.created_at).toISOString() : '—'}</td>
							<td class="mono"
								>{t.last_used ? new Date(t.last_used).toISOString() : '—'}</td
							>
							<td>
								<button type="button" class="btn ghost" onclick={() => onRevoke(t.id)}>
									Revoke
								</button>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		{/if}

		<p class="muted" style="margin-top:1.25rem;font-size:0.9rem">
			Usage:
			<code class="mono">Authorization: Bearer str_live_…</code>
		</p>
	{/if}
</section>

<style>
	label {
		display: flex;
		flex-direction: column;
		gap: 0.35rem;
		font-size: 0.9rem;
		max-width: 20rem;
	}
	input {
		font: inherit;
		padding: 0.45rem 0.6rem;
		border: 1px solid var(--line);
		border-radius: 6px;
		background: #fff;
	}
	.created code {
		display: inline-block;
		margin-top: 0.35rem;
		padding: 0.45rem 0.55rem;
		background: #f4f7fa;
		border-radius: 6px;
		word-break: break-all;
	}
	table {
		width: 100%;
		border-collapse: collapse;
		margin-top: 1.25rem;
		font-size: 0.9rem;
	}
	th,
	td {
		text-align: left;
		padding: 0.55rem 0.4rem;
		border-bottom: 1px solid var(--line);
	}
</style>
