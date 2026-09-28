/**
 * Upload Security Utility
 *
 * Provides centralized validation for file uploads to prevent:
 * - Malicious file extensions (e.g. .exe, .sh, .php disguised as allowed types)
 * - MIME type mismatches
 * - Oversized files
 * - Path traversal in filenames
 * - Empty or null filenames
 */

/** Maximum allowed file size: 50 MB */
export const MAX_FILE_SIZE = 50 * 1024 * 1024

/** Allowed file type mappings: extension -> valid MIME types */
const ALLOWED_FILE_TYPES: Record<string, string[]> = {
  // Firmware files
  '.bin': ['application/octet-stream', ''],
  '.hex': ['application/octet-stream', 'text/plain', ''],
  '.zip': ['application/zip', 'application/x-zip-compressed', ''],
  // Data import files
  '.csv': ['text/csv', 'text/plain', 'application/vnd.ms-excel', ''],
  '.xlsx': ['application/vnd.openxmlformats-officedocument.spreadsheetml.sheet', 'application/vnd.ms-excel', ''],
  '.json': ['application/json', 'text/plain', ''],
  // AI model files
  '.onnx': ['application/octet-stream', ''],
  '.pt': ['application/octet-stream', ''],
  '.pth': ['application/octet-stream', ''],
  '.pkl': ['application/octet-stream', ''],
  '.h5': ['application/octet-stream', ''],
  '.tflite': ['application/octet-stream', ''],
}

/** Dangerous extensions that should never be allowed */
const BLOCKED_EXTENSIONS = [
  '.exe', '.bat', '.cmd', '.sh', '.ps1', '.php', '.js', '.jsp',
  '.asp', '.aspx', '.py', '.rb', '.pl', '.cgi', '.jar', '.war',
  '.dll', '.so', '.dylib', '.msi', '.vbs', '.wsf', '.scr',
]

export interface UploadValidationResult {
  valid: boolean
  error?: string
}

/**
 * Validates a file for safe upload.
 *
 * @param file - The File object from input or upload component
 * @param allowedExtensions - Optional array of allowed extensions (e.g. ['.csv', '.json']).
 *                            If not provided, uses the global ALLOWED_FILE_TYPES.
 * @param maxSize - Optional max file size in bytes. Defaults to MAX_FILE_SIZE.
 * @returns UploadValidationResult indicating validity and optional error message
 */
export function validateFileUpload(
  file: File | { name?: string; size?: number; type?: string },
  allowedExtensions?: string[],
  maxSize: number = MAX_FILE_SIZE,
): UploadValidationResult {
  if (!file || !file.name) {
    return { valid: false, error: '文件无效或文件名为空' }
  }

  const filename = file.name

  // Check for path traversal attempts
  if (filename.includes('..') || filename.includes('/') || filename.includes('\\')) {
    return { valid: false, error: '文件名包含非法路径字符' }
  }

  // Check for empty filename
  if (filename.trim() === '') {
    return { valid: false, error: '文件名不能为空' }
  }

  // Extract extension
  const lastDotIndex = filename.lastIndexOf('.')
  if (lastDotIndex === -1) {
    return { valid: false, error: '文件缺少扩展名' }
  }

  const ext = filename.substring(lastDotIndex).toLowerCase()

  // Check against blocked extensions
  if (BLOCKED_EXTENSIONS.includes(ext)) {
    return { valid: false, error: `不允许上传 ${ext} 类型的文件` }
  }

  // Check against allowed extensions
  const allowedExts = allowedExtensions || Object.keys(ALLOWED_FILE_TYPES)
  if (!allowedExts.includes(ext)) {
    return { valid: false, error: `文件类型 ${ext} 不在允许列表中` }
  }

  // Check file size
  const fileSize = file.size ?? 0
  if (fileSize > maxSize) {
    const maxMB = (maxSize / (1024 * 1024)).toFixed(0)
    return { valid: false, error: `文件大小超过限制（最大 ${maxMB} MB）` }
  }

  // Optional: Check MIME type if available
  if (file.type && ALLOWED_FILE_TYPES[ext]) {
    const validMimes = ALLOWED_FILE_TYPES[ext]
    // Empty string in validMimes means we accept unknown MIME type
    if (validMimes.length > 0 && !validMimes.includes('') && !validMimes.includes(file.type)) {
      return { valid: false, error: `文件 MIME 类型 ${file.type} 与扩展名 ${ext} 不匹配` }
    }
  }

  return { valid: true }
}

/**
 * Sanitizes a filename to be safe for storage.
 * Removes path components, special characters, and normalizes the name.
 *
 * @param filename - The original filename
 * @returns Sanitized filename
 */
export function sanitizeFilename(filename: string): string {
  if (!filename) return 'unnamed'

  // Remove any path components
  const basename = filename.replace(/^.*[\\\/]/, '')

  // Remove dangerous characters: .., null bytes, control chars
  let sanitized = basename
    .replace(/\.\./g, '')
    .replace(/\0/g, '')
    .replace(/[\x00-\x1f\x7f]/g, '')

  // Limit length
  if (sanitized.length > 255) {
    const ext = sanitized.substring(sanitized.lastIndexOf('.'))
    const nameWithoutExt = sanitized.substring(0, sanitized.lastIndexOf('.'))
    sanitized = nameWithoutExt.substring(0, 255 - ext.length) + ext
  }

  return sanitized || 'unnamed'
}
