import type { AssignmentSet, MemberStatus } from '$lib/gen/strategyplatform/v1/assignmentset_pb';

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

/**
 * The set's catalog / artifact family (`spec.strategy`). Not an assignment
 * slot name: a member's slot is `member.name`, so never build a
 * /machines/<id>/<slot> link from this.
 */
export function setStrategy(c: AssignmentSet): string {
	return c.spec?.strategy?.trim() || 'nats';
}

export function memberRowKey(machine: string, name: string): string {
	return `${machine}\0${name}`;
}

export function memberStatusOf(
	c: AssignmentSet,
	machine: string,
	name: string
): MemberStatus | undefined {
	return c.status?.members?.find((s) => s.machine === machine && s.name === name);
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
