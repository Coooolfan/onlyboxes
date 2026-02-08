package com.coooolfan.onlyboxes.app

import com.fasterxml.jackson.databind.ObjectMapper
import com.fasterxml.jackson.databind.json.JsonMapper
import com.coooolfan.onlyboxes.core.service.CodeExecutor
import com.coooolfan.onlyboxes.core.service.StatefulCodeExecutorService
import com.coooolfan.onlyboxes.infra.boxlite.BoxliteBoxFactory
import org.springframework.ai.tool.ToolCallbackProvider
import org.springframework.ai.tool.method.MethodToolCallbackProvider
import org.springframework.beans.factory.annotation.Value
import org.springframework.boot.autoconfigure.SpringBootApplication
import org.springframework.boot.runApplication
import org.springframework.context.annotation.Bean

@SpringBootApplication(proxyBeanMethods = false)
class App {
    @Bean("mcpServerObjectMapper")
    fun mcpServerObjectMapper(): ObjectMapper {
        return JsonMapper
            .builder()
            .findAndAddModules()
            .build()
    }

    @Bean
    fun codeExecutor(
        @Value("\${onlyboxes.lease.default-seconds:30}")
        defaultLeaseSeconds: Long,
    ): CodeExecutor {
        return StatefulCodeExecutorService(
            boxFactory = BoxliteBoxFactory(),
            defaultLeaseSeconds = defaultLeaseSeconds,
        )
    }

    @Bean
    fun mcpController(
        codeExecutor: CodeExecutor,
        @Value("\${onlyboxes.lease.default-seconds:30}")
        defaultLeaseSeconds: Long,
    ): McpController {
        return McpController(
            codeExecutor = codeExecutor,
            defaultLeaseSeconds = defaultLeaseSeconds,
        )
    }

    @Bean
    fun toolCallbackProvider(mcpController: McpController): ToolCallbackProvider {
        return MethodToolCallbackProvider
            .builder()
            .toolObjects(mcpController)
            .build()
    }

    companion object {
        @JvmStatic
        fun main(args: Array<String>) {
            runApplication<App>(*args)
        }
    }
}
