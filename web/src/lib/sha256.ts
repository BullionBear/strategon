/** Files larger than this skip in-browser hashing to avoid OOM on docker-save tars. */
export const AUTO_HASH_MAX_BYTES = 64 * 1024 * 1024;

export class CryptoUnavailableError extends Error {
	constructor() {
		super(
			'Web Crypto is unavailable (this page is not a secure origin). Paste the sha256sum digest instead.'
		);
		this.name = 'CryptoUnavailableError';
	}
}

export class FileTooLargeToHashError extends Error {
	constructor(size: number) {
		const miB = Math.max(1, Math.round(size / (1024 * 1024)));
		super(
			`File is ${miB} MiB — paste the sha256sum digest instead of hashing in the browser.`
		);
		this.name = 'FileTooLargeToHashError';
	}
}

export function webCryptoSubtle(): SubtleCrypto | undefined {
	return globalThis.crypto?.subtle;
}

/** Stream the file in chunks (never file.arrayBuffer) and SHA-256 via Web Crypto. */
export async function sha256File(file: File): Promise<string> {
	const subtle = webCryptoSubtle();
	if (!subtle) {
		throw new CryptoUnavailableError();
	}
	if (file.size > AUTO_HASH_MAX_BYTES) {
		throw new FileTooLargeToHashError(file.size);
	}
	const buf = await readFileCapped(file, AUTO_HASH_MAX_BYTES);
	const hash = await subtle.digest('SHA-256', buf);
	const hex = [...new Uint8Array(hash)].map((b) => b.toString(16).padStart(2, '0')).join('');
	return `sha256:${hex}`;
}

async function readFileCapped(file: File, maxBytes: number): Promise<Uint8Array> {
	if (file.size > maxBytes) {
		throw new FileTooLargeToHashError(file.size);
	}
	const out = new Uint8Array(file.size);
	let offset = 0;
	const reader = file.stream().getReader();
	try {
		while (true) {
			const { done, value } = await reader.read();
			if (done) break;
			if (offset + value.byteLength > maxBytes) {
				throw new FileTooLargeToHashError(offset + value.byteLength);
			}
			out.set(value, offset);
			offset += value.byteLength;
		}
	} finally {
		reader.releaseLock();
	}
	return out;
}
