import type { AssignmentSet } from '$lib/gen/strategyplatform/v1/assignmentset_pb';

export function setPhaseClass(phase: string | undefined): string {
	switch (phase) {
		case 'Ready':
			return 'ok';
		case 'Failed':
		case 'Degraded':
			return 'bad';
		case 'Deleting':
			return 'off';
		case 'Rolling':
		case 'Pending':
			return 'lag';
		default:
			return '';
	}
}

export function setGenerationLag(c: AssignmentSet): boolean {
	const gen = c.metadata?.generation ?? 0n;
	const obs = c.status?.observedGeneration ?? 0n;
	return gen !== obs;
}

export function setStrategy(c: AssignmentSet): string {
	return c.spec?.strategy?.trim() || 'nats';
}

/** Controller stores proto enum names (DEPLOY_PHASE_HEALTHY). */
export function memberPhaseLabel(phase: string | undefined): string {
	const raw = (phase ?? '').trim();
	if (!raw || raw === '—') return '—';
	const trimmed = raw.replace(/^DEPLOY_PHASE_/, '');
	return trimmed
		.toLowerCase()
		.split('_')
		.filter(Boolean)
		.map((w) => w.charAt(0).toUpperCase() + w.slice(1))
		.join(' ');
}
