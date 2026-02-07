package com.coooolfan.onlyboxes.app

import com.coooolfan.onlyboxes.core.service.CodeExecutor
import com.coooolfan.onlyboxes.core.service.StatefulCodeExecutorService
import com.coooolfan.onlyboxes.infra.boxlite.BoxliteBoxFactory

internal object RuntimeRegistry {
    val codeExecutor: CodeExecutor by lazy {
        StatefulCodeExecutorService(BoxliteBoxFactory())
    }
}
