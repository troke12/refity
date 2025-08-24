package web

const dashboardTemplate = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Refity Docker Registry Dashboard</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script defer src="https://cdn.jsdelivr.net/npm/alpinejs@3.x.x/dist/cdn.min.js"></script>
</head>
<body class="bg-gray-50">
    <div class="min-h-screen" x-data="dashboard()">
        <!-- Header -->
        <header class="bg-white shadow">
            <div class="max-w-7xl mx-auto px-4 sm:px-6 lg:px-8">
                <div class="flex justify-between h-16">
                    <div class="flex items-center">
                        <h1 class="text-xl font-semibold text-gray-900">Refity Docker Registry</h1>
                    </div>
                    <div class="flex items-center space-x-4">
                        <button @click="refreshData()" class="bg-blue-500 hover:bg-blue-700 text-white font-bold py-2 px-4 rounded">
                            Refresh
                        </button>
                    </div>
                </div>
            </div>
        </header>

        <div class="max-w-7xl mx-auto py-6 sm:px-6 lg:px-8">
            <!-- Statistics Cards -->
            <div class="grid grid-cols-1 md:grid-cols-3 gap-6 mb-8">
                <div class="bg-white overflow-hidden shadow rounded-lg">
                    <div class="p-5">
                        <div class="flex items-center">
                            <div class="flex-shrink-0">
                                <svg class="h-6 w-6 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
                                </svg>
                            </div>
                            <div class="ml-5 w-0 flex-1">
                                <dl>
                                    <dt class="text-sm font-medium text-gray-500 truncate">Total Images</dt>
                                    <dd class="text-lg font-medium text-gray-900">{{.TotalImages}}</dd>
                                </dl>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="bg-white overflow-hidden shadow rounded-lg">
                    <div class="p-5">
                        <div class="flex items-center">
                            <div class="flex-shrink-0">
                                <svg class="h-6 w-6 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
                                </svg>
                            </div>
                            <div class="ml-5 w-0 flex-1">
                                <dl>
                                    <dt class="text-sm font-medium text-gray-500 truncate">Total Repositories</dt>
                                    <dd class="text-lg font-medium text-gray-900">{{len .Repositories}}</dd>
                                </dl>
                            </div>
                        </div>
                    </div>
                </div>

                <div class="bg-white overflow-hidden shadow rounded-lg">
                    <div class="p-5">
                        <div class="flex items-center">
                            <div class="flex-shrink-0">
                                <svg class="h-6 w-6 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                                    <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M9 19v-6a2 2 0 00-2-2H5a2 2 0 00-2 2v6a2 2 0 002 2h2a2 2 0 002-2zm0 0V9a2 2 0 012-2h2a2 2 0 012 2v10m-6 0a2 2 0 002 2h2a2 2 0 002-2m0 0V5a2 2 0 012-2h2a2 2 0 012 2v14a2 2 0 01-2 2h-2a2 2 0 01-2-2z" />
                                </svg>
                            </div>
                            <div class="ml-5 w-0 flex-1">
                                <dl>
                                    <dt class="text-sm font-medium text-gray-500 truncate">Total Size</dt>
                                    <dd class="text-lg font-medium text-gray-900">{{formatBytes .TotalSize}}</dd>
                                </dl>
                            </div>
                        </div>
                    </div>
                </div>
            </div>

            <!-- Create Repository Section -->
            <div class="bg-white shadow rounded-lg mb-8">
                <div class="px-4 py-5 sm:p-6">
                    <h3 class="text-lg leading-6 font-medium text-gray-900 mb-4">Create New Repository</h3>
                    <div class="flex space-x-4">
                        <input type="text" x-model="newRepoName" placeholder="Enter repository name (e.g., myapp)" 
                               class="flex-1 shadow-sm focus:ring-blue-500 focus:border-blue-500 block w-full sm:text-sm border-gray-300 rounded-md">
                        <button @click="createRepository()" 
                                class="inline-flex items-center px-4 py-2 border border-transparent text-sm font-medium rounded-md shadow-sm text-white bg-blue-600 hover:bg-blue-700 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-blue-500">
                            Create Repository
                        </button>
                    </div>
                    <div x-show="createMessage" x-text="createMessage" 
                         :class="createSuccess ? 'text-green-600' : 'text-red-600'" 
                         class="mt-2 text-sm"></div>
                </div>
            </div>

            <!-- Repositories List -->
            <div class="bg-white shadow overflow-hidden sm:rounded-md">
                <div class="px-4 py-5 sm:px-6">
                    <h3 class="text-lg leading-6 font-medium text-gray-900">Repositories</h3>
                    <p class="mt-1 max-w-2xl text-sm text-gray-500">List of all repositories and their tags</p>
                </div>
                <ul class="divide-y divide-gray-200">
                    {{range .Repositories}}
                    <li class="px-4 py-4">
                        <div class="flex items-center justify-between">
                            <div class="flex-1">
                                <h4 class="text-lg font-medium text-gray-900">{{.Name}}</h4>
                                <div class="mt-2 flex flex-wrap gap-2">
                                    {{if .Tags}}
                                        {{range .Tags}}
                                        <span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-blue-100 text-blue-800">
                                            {{.Name}} ({{formatBytes .Size}})
                                            <button @click="deleteTag('{{$.Name}}', '{{.Name}}')" 
                                                    class="ml-1 text-blue-600 hover:text-blue-800">
                                                ×
                                            </button>
                                        </span>
                                        {{end}}
                                    {{else}}
                                        <span class="inline-flex items-center px-2.5 py-0.5 rounded-full text-xs font-medium bg-gray-100 text-gray-600">
                                            No images yet
                                        </span>
                                    {{end}}
                                </div>
                            </div>
                            <div class="flex space-x-2">
                                <button @click="deleteRepository('{{.Name}}')" 
                                        class="inline-flex items-center px-3 py-1 border border-transparent text-sm leading-4 font-medium rounded-md text-red-700 bg-red-100 hover:bg-red-200 focus:outline-none focus:ring-2 focus:ring-offset-2 focus:ring-red-500">
                                    Delete Repository
                                </button>
                            </div>
                        </div>
                    </li>
                    {{end}}
                </ul>
                {{if not .Repositories}}
                <div class="px-4 py-8 text-center text-gray-500">
                    <svg class="mx-auto h-12 w-12 text-gray-400" fill="none" viewBox="0 0 24 24" stroke="currentColor">
                        <path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M19 11H5m14 0a2 2 0 012 2v6a2 2 0 01-2 2H5a2 2 0 01-2-2v-6a2 2 0 012-2m14 0V9a2 2 0 00-2-2M5 11V9a2 2 0 012-2m0 0V5a2 2 0 012-2h6a2 2 0 012 2v2M7 7h10" />
                    </svg>
                    <h3 class="mt-2 text-sm font-medium text-gray-900">No repositories</h3>
                    <p class="mt-1 text-sm text-gray-500">Create a repository to get started.</p>
                </div>
                {{end}}
            </div>
        </div>
    </div>

    <script>
        function dashboard() {
            return {
                newRepoName: '',
                createMessage: '',
                createSuccess: false,
                
                async createRepository() {
                    if (!this.newRepoName.trim()) {
                        this.createMessage = 'Repository name is required';
                        this.createSuccess = false;
                        return;
                    }

                    try {
                        const response = await fetch('/api/repositories', {
                            method: 'POST',
                            headers: {
                                'Content-Type': 'application/json',
                            },
                            body: JSON.stringify({ name: this.newRepoName.trim() })
                        });

                        const result = await response.json();
                        
                        if (response.ok) {
                            this.createMessage = result.message;
                            this.createSuccess = true;
                            this.newRepoName = '';
                            setTimeout(() => {
                                this.createMessage = '';
                                window.location.reload();
                            }, 2000);
                        } else {
                            this.createMessage = result.message || 'Failed to create repository';
                            this.createSuccess = false;
                        }
                    } catch (error) {
                        this.createMessage = 'Error creating repository';
                        this.createSuccess = false;
                    }
                },

                async deleteRepository(repoName) {
                    if (!confirm('Are you sure you want to delete repository "' + repoName + '" and all its tags?')) {
                        return;
                    }

                    try {
                        const response = await fetch('/api/repositories/' + encodeURIComponent(repoName), {
                            method: 'DELETE'
                        });

                        if (response.ok) {
                            window.location.reload();
                        } else {
                            const result = await response.json();
                            alert('Failed to delete repository: ' + (result.message || 'Unknown error'));
                        }
                    } catch (error) {
                        alert('Error deleting repository');
                    }
                },

                async deleteTag(repoName, tagName) {
                    if (!confirm('Are you sure you want to delete tag "' + tagName + '" from repository "' + repoName + '"?')) {
                        return;
                    }

                    try {
                        const response = await fetch('/api/repositories/' + encodeURIComponent(repoName) + '/tags/' + encodeURIComponent(tagName), {
                            method: 'DELETE'
                        });

                        if (response.ok) {
                            window.location.reload();
                        } else {
                            const result = await response.json();
                            alert('Failed to delete tag: ' + (result.message || 'Unknown error'));
                        }
                    } catch (error) {
                        alert('Error deleting tag');
                    }
                },

                refreshData() {
                    window.location.reload();
                }
            }
        }
    </script>
</body>
</html>
`
