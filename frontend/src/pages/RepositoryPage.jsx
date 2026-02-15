import { useState, useEffect } from 'react';
import { useParams, Link, useNavigate } from 'react-router-dom';
import { groupsAPI, repositoriesAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import { formatDate } from '../utils/formatDate';
import Navbar from '../components/Navbar';
import Footer from '../components/Footer';
import './RepositoryPage.css';

function getRegistryHost() {
  const fromEnv = import.meta.env.VITE_REGISTRY_URL;
  if (fromEnv) return fromEnv.replace(/\/$/, '');
  if (import.meta.env.PROD) return window.location.host;
  return 'localhost:5000';
}

function RepositoryPage() {
  const { groupName, repoName } = useParams();
  const navigate = useNavigate();
  const decodedGroup = decodeURIComponent(groupName || '');
  const decodedRepo = decodeURIComponent(repoName || '');
  const [data, setData] = useState(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);
  const [copiedTag, setCopiedTag] = useState(null);
  const [isDeletingRepo, setIsDeletingRepo] = useState(false);

  const loadData = async () => {
    try {
      const repoData = await groupsAPI.getTags(decodedGroup, decodedRepo);
      setData(repoData);
    } catch (error) {
      console.error('Failed to load repository data:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
  }, [groupName, repoName]);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };


  const registryHost = getRegistryHost();
  const fullImage = `${registryHost}/${decodedGroup}/${decodedRepo}`;

  const getDockerPullCmd = (tagName) => `docker pull ${fullImage}:${tagName}`;

  const handleCopyDockerPull = async (tagName) => {
    try {
      await navigator.clipboard.writeText(getDockerPullCmd(tagName));
      setCopiedTag(tagName);
      setTimeout(() => setCopiedTag(null), 2000);
    } catch (e) {
      console.error(e);
    }
  };

  const fullRepoName = `${decodedGroup}/${decodedRepo}`;

  const handleDeleteTag = async (tagName) => {
    if (!window.confirm(`Are you sure you want to delete tag "${tagName}"?`)) {
      return;
    }

    try {
      await repositoriesAPI.deleteTag(fullRepoName, tagName);
      loadData();
    } catch (error) {
      console.error('Failed to delete tag:', error);
      alert('Failed to delete tag');
    }
  };

  const handleDeleteRepository = async () => {
    if (isDeletingRepo) return;
    if (!window.confirm(`Delete this repository and all its tags?\n\nThis will also remove the folder from SFTP. This cannot be undone.`)) return;
    setIsDeletingRepo(true);
    try {
      await repositoriesAPI.delete(fullRepoName);
      navigate(`/group/${encodeURIComponent(decodedGroup)}`);
    } catch (error) {
      console.error('Failed to delete repository:', error);
      alert('Failed to delete repository: ' + (error.response?.data?.message || error.message));
    } finally {
      setIsDeletingRepo(false);
    }
  };

  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  return (
    <div className="container-main">
      <Navbar onRefresh={handleRefresh} isRefreshing={isRefreshing} title={`${decodedGroup}/${decodedRepo}`} />

      <div className="container-fluid px-0">
        <div className="card">
          <div className="card-header">
            <div className="card-header-content">
              <div className="card-header-title">
                <Link to={`/group/${encodeURIComponent(decodedGroup)}`} className="nav-link-back">
                  <i className="bi bi-arrow-left"></i>
                </Link>
                <h5>
                  <i className="bi bi-box-seam"></i>
                  {decodedGroup}/{decodedRepo}
                </h5>
                <button
                  type="button"
                  onClick={handleDeleteRepository}
                  disabled={isDeletingRepo}
                  className="btn btn-link text-danger p-0 ms-2"
                  title="Delete repository"
                  aria-label="Delete repository"
                >
                  {isDeletingRepo ? (
                    <span className="spinner-border spinner-border-sm" style={{ width: '1rem', height: '1rem' }} aria-hidden="true" />
                  ) : (
                    <i className="bi bi-trash"></i>
                  )}
                </button>
              </div>
              <small>List of tags for this repository</small>
            </div>
          </div>
          <div className="card-body">
            {data?.tags && data.tags.length > 0 ? (
              <div className="table-responsive">
                <table className="table table-hover">
                  <thead>
                    <tr>
                      <th>Tag</th>
                      <th>Size</th>
                      <th>Date Created</th>
                      <th>Docker pull</th>
                      <th>Actions</th>
                    </tr>
                  </thead>
                  <tbody>
                    {data.tags.map((tag) => (
                      <tr key={tag.name}>
                        <td>
                          <span className="badge bg-primary">
                            <i className="bi bi-tag me-1"></i>
                            {tag.name}
                          </span>
                        </td>
                        <td>{formatBytes(tag.size)}</td>
                        <td>
                          <small className="text-muted">
                            <i className="bi bi-calendar3 me-1"></i>
                            {formatDate(tag.created_at)}
                          </small>
                        </td>
                        <td>
                          <div className="docker-pull-cell">
                            <code className="docker-pull-row-cmd">{getDockerPullCmd(tag.name)}</code>
                            <button
                              type="button"
                              className="btn btn-sm btn-outline-primary"
                              onClick={() => handleCopyDockerPull(tag.name)}
                              title="Copy docker pull command"
                            >
                              {copiedTag === tag.name ? <i className="bi bi-check2"></i> : <i className="bi bi-clipboard"></i>}
                              {copiedTag === tag.name ? ' Copied' : ' Copy'}
                            </button>
                          </div>
                        </td>
                        <td>
                          <button
                            onClick={() => handleDeleteTag(tag.name)}
                            className="btn btn-sm btn-danger"
                            title="Delete tag"
                          >
                            <i className="bi bi-trash"></i>
                          </button>
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : (
              <p className="text-muted mb-0">No tags found for this repository.</p>
            )}
          </div>
        </div>
      </div>
      
      <Footer />
    </div>
  );
}

export default RepositoryPage;

