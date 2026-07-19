<script lang="ts">
	type Point = { x: number; y: number };

	let {
		values = [],
		width = 120,
		height = 28,
		stroke = 'var(--accent)',
		fill = 'rgba(13, 115, 119, 0.12)'
	}: {
		values?: number[];
		width?: number;
		height?: number;
		stroke?: string;
		fill?: string;
	} = $props();

	const path = $derived.by(() => {
		if (!values.length) return { line: '', area: '' };
		const min = Math.min(...values);
		const max = Math.max(...values);
		const span = max - min || 1;
		const pad = 2;
		const pts: Point[] = values.map((v, i) => ({
			x: pad + (i / Math.max(values.length - 1, 1)) * (width - pad * 2),
			y: height - pad - ((v - min) / span) * (height - pad * 2)
		}));
		const line = pts.map((p, i) => `${i === 0 ? 'M' : 'L'}${p.x.toFixed(1)},${p.y.toFixed(1)}`).join(' ');
		const area = `${line} L${pts[pts.length - 1].x.toFixed(1)},${height} L${pts[0].x.toFixed(1)},${height} Z`;
		return { line, area };
	});
</script>

{#if values.length < 2}
	<svg class="spark" {width} {height} aria-hidden="true">
		<line x1="0" y1={height / 2} x2={width} y2={height / 2} class="empty" />
	</svg>
{:else}
	<svg class="spark" {width} {height} viewBox="0 0 {width} {height}" aria-hidden="true">
		<path d={path.area} fill={fill} />
		<path d={path.line} fill="none" {stroke} stroke-width="1.5" stroke-linecap="round" stroke-linejoin="round" />
	</svg>
{/if}

<style>
	.spark {
		display: block;
		overflow: visible;
	}
	.empty {
		stroke: var(--line);
		stroke-width: 1;
		stroke-dasharray: 3 3;
	}
</style>
