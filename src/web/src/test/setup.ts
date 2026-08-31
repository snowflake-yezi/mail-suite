import '@testing-library/jest-dom/vitest'

import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

// cleanup 保证每个 Vitest 用例都从空 DOM 开始，避免状态跨测试泄漏。
afterEach(() => cleanup())
