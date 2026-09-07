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

/** The picked File changed on disk between selection and hashing. */
export class FileChangedWhileHashingError extends Error {
	constructor() {
		super('File changed while it was being read — re-select it and try again.');
		this.name = 'FileChangedWhileHashingError';
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

async function readFileCapped(file: File, maxBytes: number): Promise<Uint8Array<ArrayBuffer>> {
	if (file.size > maxBytes) {
		throw new FileTooLargeToHashError(file.size);
	}
	const out = new Uint8Array(new ArrayBuffer(file.size));
	let offset = 0;
	const reader = file.stream().getReader();
	try {
		while (true) {
			const { done, value } = await reader.read();
			if (done) break;
			// `out` is sized from file.size, so a stream that outruns it means
			// the file grew after it was picked. Bail rather than let `set`
			// throw a bare RangeError.
			if (offset + value.byteLength > out.length) {
				throw new FileChangedWhileHashingError();
			}
			out.set(value, offset);
			offset += value.byteLength;
		}
	} finally {
		reader.releaseLock();
	}
	// A short stream leaves the tail zero-filled, which would hand back a
	// digest for bytes the file never had — and one no agent can verify.
	if (offset !== out.length) {
		throw new FileChangedWhileHashingError();
	}
	return out;
}
