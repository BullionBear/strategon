import type { Timestamp } from '@bufbuild/protobuf/wkt';
import type { Machine } from '$lib/gen/strategyplatform/v1/control_service_pb';

/** Shared layout breakpoints (match app.css). */
export const BP = {
	mobileMax: 639,
	tabletMin: 640,
	tabletMax: 1024,
	desktopMin: 1025
} as const;

export type MachineStatusKind = 'unreachable' | 'diverging' | 'ok';

export type MachineStatus = {
	kind: MachineStatusKind;
	label: string;
	/** Lower = worse; used for default sort (anomalies first). */
	rank: number;
};

export function machineId(m: Machine): string {
	return m.metadata?.uid || m.metadata?.name || '?';
}

export function machineRegion(m: Machine): string {
	const r = m.spec?.region?.trim();
	return r || 'Unassigned';
}

export function machineStatus(m: Machine): MachineStatus {
	if (!m.reachable) {
		return { kind: 'unreachable', label: 'unreachable', rank: 0 };
	}
	const diverging = m.strategies.some((s) => !s.converged);
	if (diverging) {
		return { kind: 'diverging', label: 'diverging', rank: 1 };
	}
	return { kind: 'ok', label: 'healthy', rank: 2 };
}

export function statusPillClass(kind: MachineStatusKind): string {
	switch (kind) {
		case 'unreachable':
			return 'bad';
		case 'diverging':
			return 'lag';
		default:
			return 'ok';
	}
}

export function cpuPercent(m: Machine): number | null {
	const v = m.lastResources?.cpuPercent;
	if (v == null || Number.isNaN(v)) return null;
	return Math.max(0, Math.min(100, v));
}

export function memoryPercent(m: Machine): number | null {
	const used = m.lastResources?.memoryUsedBytes;
	const total = m.lastResources?.memoryTotalBytes || m.spec?.memoryTotalBytes;
	if (used == null || total == null || total <= 0n) return null;
	const pct = (Number(used) / Number(total)) * 100;
	if (Number.isNaN(pct)) return null;
	return Math.max(0, Math.min(100, pct));
}

export function barTone(pct: number | null): 'ok' | 'warn' | 'danger' | 'empty' {
	if (pct == null) return 'empty';
	if (pct >= 90) return 'danger';
	if (pct >= 75) return 'warn';
	return 'ok';
}

export function heartbeatMs(ts?: Timestamp): number {
	if (!ts) return 0;
	return Number(ts.seconds) * 1000 + Math.floor(Number(ts.nanos ?? 0) / 1e6);
}

export function formatHeartbeat(ts?: Timestamp, now = Date.now()): string {
	const ms = heartbeatMs(ts);
	if (!ms) return '—';
	const sec = Math.max(0, Math.floor((now - ms) / 1000));
	if (sec < 60) return `${sec}s ago`;
	if (sec < 3600) return `${Math.floor(sec / 60)}m ago`;
	if (sec < 86400) return `${Math.floor(sec / 3600)}h ago`;
	return `${Math.floor(sec / 86400)}d ago`;
}

/** Format a duration as compact uptime (e.g. 3h20m). */
export function formatUptime(startedAt?: Timestamp, now = Date.now()): string {
	const ms = heartbeatMs(startedAt);
	if (!ms) return '—';
	let sec = Math.max(0, Math.floor((now - ms) / 1000));
	const d = Math.floor(sec / 86400);
	sec %= 86400;
	const h = Math.floor(sec / 3600);
	sec %= 3600;
	const m = Math.floor(sec / 60);
	if (d > 0) return `${d}d${h}h`;
	if (h > 0) return `${h}h${m.toString().padStart(2, '0')}m`;
	if (m > 0) return `${m}m`;
	return `${sec}s`;
}

export function formatClock(ts?: Timestamp): string {
	const ms = heartbeatMs(ts);
	if (!ms) return '—';
	const d = new Date(ms);
	return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
}

export function formatBytes(n?: bigint | number | null): string {
	if (n == null) return '—';
	const v = typeof n === 'bigint' ? Number(n) : n;
	if (!Number.isFinite(v) || v < 0) return '—';
	const units = ['B', 'KB', 'MB', 'GB', 'TB'];
	let x = v;
	let i = 0;
	while (x >= 1024 && i < units.length - 1) {
		x /= 1024;
		i++;
	}
	return `${x < 10 && i > 0 ? x.toFixed(1) : Math.round(x)}${units[i]}`;
}

export type SortKey =
	| 'name'
	| 'status'
	| 'reachable'
	| 'agent'
	| 'cpu'
	| 'memory'
	| 'strategies'
	| 'heartbeat';

export type SortDir = 'asc' | 'desc';

function cmpNum(a: number, b: number): number {
	return a === b ? 0 : a < b ? -1 : 1;
}

function cmpStr(a: string, b: string): number {
	return a.localeCompare(b, undefined, { sensitivity: 'base' });
}

export function compareMachines(a: Machine, b: Machine, key: SortKey, dir: SortDir): number {
	const mul = dir === 'asc' ? 1 : -1;
	let r = 0;
	switch (key) {
		case 'name':
			r = cmpStr(machineId(a), machineId(b));
			break;
		case 'status':
			r = cmpNum(machineStatus(a).rank, machineStatus(b).rank);
			if (r === 0) r = cmpStr(machineId(a), machineId(b));
			break;
		case 'reachable':
			r = cmpNum(a.reachable ? 1 : 0, b.reachable ? 1 : 0);
			break;
		case 'agent':
			r = cmpStr(
				`${a.agentVersion}:${a.agentBuildVersion}`,
				`${b.agentVersion}:${b.agentBuildVersion}`
			);
			break;
		case 'cpu':
			r = cmpNum(cpuPercent(a) ?? -1, cpuPercent(b) ?? -1);
			break;
		case 'memory':
			r = cmpNum(memoryPercent(a) ?? -1, memoryPercent(b) ?? -1);
			break;
		case 'strategies':
			r = cmpNum(a.strategies.length, b.strategies.length);
			break;
		case 'heartbeat':
			r = cmpNum(heartbeatMs(a.lastHeartbeat), heartbeatMs(b.lastHeartbeat));
			break;
	}
	if (r === 0 && key !== 'name') r = cmpStr(machineId(a), machineId(b));
	return r * mul;
}

export type RegionGroup = {
	region: string;
	machines: Machine[];
};

export function groupByRegion(machines: Machine[]): RegionGroup[] {
	const map = new Map<string, Machine[]>();
	for (const m of machines) {
		const region = machineRegion(m);
		const list = map.get(region);
		if (list) list.push(m);
		else map.set(region, [m]);
	}
	const regions = [...map.keys()].sort((a, b) => {
		if (a === 'Unassigned') return 1;
		if (b === 'Unassigned') return -1;
		return a.localeCompare(b);
	});
	return regions.map((region) => ({ region, machines: map.get(region)! }));
}
